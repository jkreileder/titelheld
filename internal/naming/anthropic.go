package naming

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Anthropic is the switchable alternative to the Vertex default.
//
// It is the one provider that needs a key, which is why LLM_API_KEY exists at
// all; with the Gemini default selected the secret is never read.
//
// Model ID, endpoint and headers were verified against Anthropic's live
// documentation on 2026-08-20, not recalled:
//
//	model:    claude-haiku-4-5-20251001 — the pinned snapshot. The docs also
//	          list "claude-haiku-4-5" as an alias, and note that for models
//	          before the 4.6 generation an alias is a convenience pointer that
//	          resolves to a dated ID. The pinned form is used here for the same
//	          reason every image in this repository is pinned by digest.
//	          https://platform.claude.com/docs/en/about-claude/models/overview
//	endpoint: POST https://api.anthropic.com/v1/messages
//	headers:  x-api-key, anthropic-version: 2023-06-01, content-type
//
// The official Go SDK is deliberately not used: adding it would be a new
// dependency, which the build spec requires asking about first, and the Vertex
// path needs raw HTTP regardless. Both providers therefore speak HTTP directly
// and no module was added to go.mod for either.

// DefaultAnthropicModel is the Haiku-class model this service ships with.
const DefaultAnthropicModel = "claude-haiku-4-5-20251001"

// anthropicVersion is the API version header value.
const anthropicVersion = "2023-06-01"

// stopReasonMaxTokens is the stop reason for a response cut off by the
// max_tokens ceiling rather than finished.
const stopReasonMaxTokens = "max_tokens"

// maxAnthropicResponseBytes caps what a decode reads from a response.
const maxAnthropicResponseBytes = 1 << 20

// Anthropic calls the Messages API.
type Anthropic struct {
	// Client is the HTTP client. Nil means a default one with a timeout.
	Client *http.Client

	// APIKey authenticates the call. Required.
	APIKey string

	// Model is the model ID. Empty means [DefaultAnthropicModel].
	Model string

	// Temperature is the sampling temperature. Zero means the spec's 0.9.
	Temperature float64

	// BaseURL overrides the endpoint host in tests.
	BaseURL string
}

// Name identifies the provider in logs.
func (a *Anthropic) Name() string { return "anthropic/" + a.model() }

func (a *Anthropic) model() string {
	if a.Model == "" {
		return DefaultAnthropicModel
	}

	return a.Model
}

func (a *Anthropic) temperature() float64 {
	if a.Temperature == 0 {
		return DefaultTemperature
	}

	return a.Temperature
}

func (a *Anthropic) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}

	return &http.Client{Timeout: defaultTimeout}
}

func (a *Anthropic) endpoint() string {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}

	return strings.TrimRight(base, "/") + "/v1/messages"
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

// Complete sends the prompt to the Messages API and returns the model's text.
func (a *Anthropic) Complete(ctx context.Context, prompt Prompt) (string, error) {
	if a.APIKey == "" {
		return "", errors.New("naming: anthropic: no API key configured")
	}

	payload := anthropicRequest{
		Model:       a.model(),
		MaxTokens:   maxOutputTokens,
		Temperature: a.temperature(),
		System:      prompt.System,
		Messages:    []anthropicMessage{{Role: roleUser, Content: prompt.User}},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("naming: anthropic: encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("naming: anthropic: build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", a.APIKey)
	request.Header.Set("Anthropic-Version", anthropicVersion)

	response, err := a.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("naming: anthropic: request: %w", err)
	}
	defer drainAndClose(response)

	if response.StatusCode != http.StatusOK {
		// The body can echo the request, which carries the prompt, so it is
		// never included.
		return "", fmt.Errorf("naming: anthropic: %w", statusError(response))
	}

	var decoded anthropicResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAnthropicResponseBytes)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("naming: anthropic: decode response: %w", err)
	}

	var text strings.Builder

	for _, block := range decoded.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	if text.Len() == 0 {
		return "", ErrNoTitle
	}

	// A truncated response is valid JSON's problem only by accident: the text
	// block holds half an object, and the parser then reports "response is not
	// JSON", naming the wrong cause. Say what actually happened — the same
	// house rule that applies to a blocked DNS request.
	if decoded.StopReason == stopReasonMaxTokens {
		return "", fmt.Errorf("naming: anthropic: response truncated at max_tokens (%d); the title did not fit",
			maxOutputTokens)
	}

	return text.String(), nil
}
