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

// OpenRouter is the third provider: one key, many narrators.
//
// It speaks the OpenAI-compatible chat-completions dialect against a
// configurable base URL, so a voice miss can be diagnosed the same day by
// re-sweeping the queued ride under a different model — a configuration
// change, not a release. The prompt is the same public-feed material the
// other two providers receive; only the processor differs.
//
// Endpoint, headers and response shape were verified against OpenRouter's
// live documentation on 2026-08-26, not recalled:
//
//	endpoint: POST {base}/chat/completions, base https://openrouter.ai/api/v1
//	headers:  Authorization: Bearer <key>; HTTP-Referer and X-OpenRouter-Title
//	          are optional attribution ("X-Title" is also accepted)
//	response: choices[0].message.content; finish_reason is normalized to
//	          stop, length, content_filter, error or tool_calls
//	https://openrouter.ai/docs/api-reference/overview
//	https://openrouter.ai/docs/api-reference/chat-completion
//
// The model was verified against the live catalog, GET
// https://openrouter.ai/api/v1/models, on the same day: anthropic/claude-haiku-4.5
// is the Haiku-class entry, and the catalog exposes no dated snapshot for it —
// the alias is as pinned as OpenRouter allows. Other narrators listed there
// that day, for an A/B: google/gemini-3.7-flash, openai/gpt-5.4-mini,
// mistralai/mistral-small-2603.
//
// No JSON mode is requested. The prompt asks for JSON and the validator
// enforces it, which is the contract every provider here works under; a
// provider-side parameter one narrator rejects would turn an A/B of models
// into an A/B of parameter support.

// DefaultOpenRouterModel is the Haiku-class model this provider ships with.
const DefaultOpenRouterModel = "anthropic/claude-haiku-4.5"

// DefaultOpenRouterBaseURL is OpenRouter's API root.
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

// finishReasonLength is the normalized finish reason for a completion cut off
// by max_tokens rather than finished.
const finishReasonLength = "length"

// maxOpenRouterResponseBytes caps what a decode reads from a response.
const maxOpenRouterResponseBytes = 1 << 20

// OpenRouter calls an OpenAI-compatible chat-completions endpoint.
type OpenRouter struct {
	// Client is the HTTP client. Nil means a default one with a timeout.
	Client *http.Client

	// APIKey authenticates the call. Required.
	APIKey string

	// Model is the model ID in the router's namespace. Empty means
	// [DefaultOpenRouterModel].
	Model string

	// Temperature is the sampling temperature. Zero means the spec's 0.9.
	Temperature float64

	// BaseURL is the API root. Empty means [DefaultOpenRouterBaseURL]; the
	// configuration layer is what requires it to be https, because the key
	// travels in a header to whatever host this names.
	BaseURL string
}

// Name identifies the provider in logs.
func (o *OpenRouter) Name() string { return "openrouter/" + o.model() }

func (o *OpenRouter) model() string {
	if o.Model == "" {
		return DefaultOpenRouterModel
	}

	return o.Model
}

func (o *OpenRouter) temperature() float64 {
	if o.Temperature == 0 {
		return DefaultTemperature
	}

	return o.Temperature
}

func (o *OpenRouter) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}

	return &http.Client{Timeout: defaultTimeout}
}

func (o *OpenRouter) endpoint() string {
	base := o.BaseURL
	if base == "" {
		base = DefaultOpenRouterBaseURL
	}

	return strings.TrimRight(base, "/") + "/chat/completions"
}

type chatRequest struct {
	Model       string        `json:"model"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Complete sends the prompt as a system and a user message and returns the
// first choice's text.
func (o *OpenRouter) Complete(ctx context.Context, prompt Prompt) (string, error) {
	if o.APIKey == "" {
		return "", errors.New("naming: openrouter: no API key configured")
	}

	payload := chatRequest{
		Model:       o.model(),
		MaxTokens:   maxOutputTokens,
		Temperature: o.temperature(),
		Messages: []chatMessage{
			{Role: roleSystem, Content: prompt.System},
			{Role: roleUser, Content: prompt.User},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("naming: openrouter: encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("naming: openrouter: build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+o.APIKey)
	request.Header.Set("HTTP-Referer", "https://github.com/jkreileder/titelheld")
	request.Header.Set("X-OpenRouter-Title", "titelheld")

	response, err := o.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("naming: openrouter: request: %w", err)
	}
	defer drainAndClose(response)

	if response.StatusCode != http.StatusOK {
		// The body can echo the request, which carries the prompt, so it is
		// never included.
		return "", fmt.Errorf("naming: openrouter: %w", statusError(response))
	}

	var decoded chatResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxOpenRouterResponseBytes)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("naming: openrouter: decode response: %w", err)
	}

	if len(decoded.Choices) == 0 {
		return "", ErrNoTitle
	}

	choice := decoded.Choices[0]

	// Before the content is looked at: a truncated response may hold half an
	// object or nothing at all, and either way the cause is the ceiling, not
	// the parser or an empty model.
	if choice.FinishReason == finishReasonLength {
		return "", fmt.Errorf("naming: openrouter: response truncated at max_tokens (%d); the title did not fit",
			maxOutputTokens)
	}

	if choice.Message.Content == "" {
		return "", ErrNoTitle
	}

	return choice.Message.Content, nil
}
