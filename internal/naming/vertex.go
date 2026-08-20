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
	"time"
)

// Gemini through Vertex AI is the default provider, and it holds no key.
//
// The runtime service account calls the regional Vertex endpoint with the
// ambient credentials Cloud Run already gives it, so there is no Gemini API key
// anywhere — not in Secret Manager, not in the environment, not in this
// repository. LLM_API_KEY exists only for the Anthropic alternative.
//
// Model ID and endpoint were verified against Google's live documentation on
// 2026-08-20, not recalled:
//
//	model:    gemini-3.7-flash
//	          https://docs.cloud.google.com/gemini-enterprise-agent-platform/models/gemini/3-7-flash
//	endpoint: POST https://LOCATION-aiplatform.googleapis.com/v1/projects/PROJECT
//	              /locations/LOCATION/publishers/google/models/MODEL:generateContent
//	          https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference
//
// Regional availability of this model in europe-west3/west4 could not be
// confirmed from the documentation, so the location is configuration rather
// than a constant and the operator confirms it once — see README.md.

// DefaultVertexModel is the Flash-class model this service ships with.
const DefaultVertexModel = "gemini-3.7-flash"

// maxVertexResponseBytes caps what a decode reads from a Vertex response. A
// title is a few dozen bytes; the ceiling is for a response that never ends.
const maxVertexResponseBytes = 1 << 20

// Vertex calls Gemini on Vertex AI.
type Vertex struct {
	// Client must already carry credentials. The caller builds it, which is
	// what keeps this package free of a cloud SDK.
	Client *http.Client

	// ProjectID, Location and Model address the endpoint.
	ProjectID string
	Location  string
	Model     string

	// Temperature is the sampling temperature. Zero means the spec's 0.9.
	Temperature float64

	// BaseURL overrides the endpoint host in tests. Empty means the real
	// regional host.
	BaseURL string
}

// Name identifies the provider in logs.
func (v *Vertex) Name() string { return "vertex/" + v.model() }

func (v *Vertex) model() string {
	if v.Model == "" {
		return DefaultVertexModel
	}

	return v.Model
}

func (v *Vertex) temperature() float64 {
	if v.Temperature == 0 {
		return DefaultTemperature
	}

	return v.Temperature
}

func (v *Vertex) endpoint() string {
	base := v.BaseURL
	if base == "" {
		base = "https://" + v.Location + "-aiplatform.googleapis.com"
	}

	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		strings.TrimRight(base, "/"), v.ProjectID, v.Location, v.model())
}

type vertexRequest struct {
	SystemInstruction *vertexContent    `json:"systemInstruction,omitempty"`
	Contents          []vertexContent   `json:"contents"`
	GenerationConfig  vertexGenerConfig `json:"generationConfig"`
}

type vertexContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []vertexPart `json:"parts"`
}

type vertexPart struct {
	Text string `json:"text"`
}

type vertexGenerConfig struct {
	Temperature float64 `json:"temperature"`
	// A title is short; this bounds a runaway response rather than shaping
	// the answer.
	MaxOutputTokens int `json:"maxOutputTokens"`
	// The API is asked for JSON, but the validator is what enforces it —
	// see the package comment.
	ResponseMimeType string `json:"responseMimeType,omitempty"`
}

type vertexResponse struct {
	Candidates []struct {
		Content vertexContent `json:"content"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// Complete sends the prompt to Vertex and returns the model's text.
func (v *Vertex) Complete(ctx context.Context, prompt Prompt) (string, error) {
	if v.Client == nil {
		return "", errors.New("naming: vertex: no HTTP client configured")
	}

	if v.ProjectID == "" || v.Location == "" {
		return "", errors.New("naming: vertex: project and location are required")
	}

	payload := vertexRequest{
		SystemInstruction: &vertexContent{Parts: []vertexPart{{Text: prompt.System}}},
		Contents:          []vertexContent{{Role: "user", Parts: []vertexPart{{Text: prompt.User}}}},
		GenerationConfig: vertexGenerConfig{
			Temperature:      v.temperature(),
			MaxOutputTokens:  maxOutputTokens,
			ResponseMimeType: "application/json",
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("naming: vertex: encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("naming: vertex: build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := v.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("naming: vertex: request: %w", err)
	}
	defer drainAndClose(response)

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("naming: vertex: %w", statusError(response))
	}

	var decoded vertexResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxVertexResponseBytes)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("naming: vertex: decode response: %w", err)
	}

	if decoded.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("naming: vertex: prompt blocked (%s)", decoded.PromptFeedback.BlockReason)
	}

	var text strings.Builder

	// One candidate is requested and the first is used; a provider that
	// returns more is not a reason to concatenate them into nonsense.
	if len(decoded.Candidates) > 0 {
		for _, part := range decoded.Candidates[0].Content.Parts {
			text.WriteString(part.Text)
		}
	}

	if text.Len() == 0 {
		return "", ErrNoTitle
	}

	return text.String(), nil
}

// DefaultTemperature is the sampling temperature the spec asks for.
const DefaultTemperature = 0.9

// maxOutputTokens bounds a response that should be one short JSON object.
const maxOutputTokens = 256

// defaultTimeout bounds a naming call. A title is not worth holding a Cloud Run
// instance open indefinitely.
const defaultTimeout = 30 * time.Second

// drainAndClose returns the connection to the pool without reading an unbounded
// body — the same treatment the Strava client gives its responses.
func drainAndClose(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	_ = response.Body.Close()
}

// statusError describes a non-200 without quoting the body back, which may
// echo the prompt.
func statusError(response *http.Response) error {
	return fmt.Errorf("unexpected status %d", response.StatusCode)
}
