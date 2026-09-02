package naming

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Gemini through Vertex AI is the default provider, and it holds no key.
//
// The runtime service account calls the regional Vertex endpoint with the
// ambient credentials Cloud Run already gives it, so there is no Gemini API key
// anywhere — not in Secret Manager, not in the environment, not in this
// repository. LLM_API_KEY exists only for the keyed alternatives, Anthropic
// and OpenRouter.
//
// The endpoint was verified against Google's live documentation on 2026-08-20:
//
//	POST https://LOCATION-aiplatform.googleapis.com/v1/projects/PROJECT
//	     /locations/LOCATION/publishers/google/models/MODEL:generateContent
//	https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference
//
// The model was not, in the end, taken from documentation at all. The model
// index lists gemini-3.7-flash as the newest Flash model, but an index is a
// catalogue and says nothing about where a model is served. Reading the
// publisher-model metadata does — the table below is a probe of 2026-08-31:
//
//	                    global   eu (REP)   europe-west3   europe-west4
//	gemini-3.7-flash    200      200        404            404
//	gemini-3.6-flash    200      200        404            404
//	gemini-3.5-flash    200      200        200 (GA)       200 (GA)
//
// The newest models exist, but no European region serves them. Two hosts
// reach them: the global endpoint, which routes to whichever region has
// capacity, and the EU multi-region, which keeps the request inside Europe.
// That matters here because the prompt carries place names derived from the
// athlete's GPS traces.
//
// The EU multi-region is spelled aiplatform.eu.rep.googleapis.com — the
// LOCATION-aiplatform pattern does not apply to it, and
// eu-aiplatform.googleapis.com serves nothing at all.
//
// The default stays the newest model served in-region; the two multi-region
// hosts are an opt-in, with the probe, in docs/configuration.md.

// DefaultVertexModel is the Flash-class model this service ships with.
const DefaultVertexModel = "gemini-3.5-flash"

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

	// Logger receives one usage line per call. Nil logs nothing. It exists
	// because thinking tokens are billed inside maxOutputTokens and Gemini
	// 3.x decides for itself how many to spend: how close a call came to the
	// ceiling must be readable from production, not inferred from whether a
	// truncation happened.
	Logger *slog.Logger
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

// Vertex's two multi-region locations, spelled out because neither host
// follows the LOCATION-aiplatform pattern every region uses.
const (
	// GlobalLocation routes to whichever region has capacity, from the bare
	// host.
	GlobalLocation = "global"

	// EULocation keeps the request inside Europe, from a regional endpoint
	// (REP) host of its own shape.
	EULocation = "eu"
)

// euBaseURL is the EU multi-region's host. "eu-aiplatform.googleapis.com",
// which the regional pattern would build, resolves but serves no model.
const euBaseURL = "https://aiplatform.eu.rep.googleapis.com"

func (v *Vertex) endpoint() string {
	base := v.BaseURL
	if base == "" {
		// Every region is served from LOCATION-aiplatform.googleapis.com. The
		// two multi-regions are not: "global" is the bare host — and
		// "global-aiplatform.googleapis.com" does not resolve at all — and
		// "eu" is the REP host above.
		switch v.Location {
		case GlobalLocation:
			base = "https://aiplatform.googleapis.com"
		case EULocation:
			base = euBaseURL
		default:
			base = "https://" + v.Location + "-aiplatform.googleapis.com"
		}
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

// vertexThinkingConfig asks for as little reasoning as the model generation
// allows.
//
// Gemini thinks by default and thinking tokens are billed inside
// maxOutputTokens. A budget of zero is honored fully by gemini-3.5-flash,
// which then thinks not at all; Gemini 3.x cannot disable thinking and reads
// the zero as a floor, spending a few hundred tokens whatever it is told. It
// is the minimum-thinking spelling on both generations, which is why one
// request shape serves both.
//
// It also fixes the output shape on 3.5-flash: thinking-on, the model prefixed
// prose and a markdown fence despite responseMimeType; at a zero budget the
// same request returns bare JSON. Naming a ride needs no chain of reasoning,
// so this is not a budget trade — it is asking for something the task never
// wanted.
//
// The budget rather than Gemini 3's thinkingLevel, verified live on both model
// generations on 2026-08-31: the field is accepted for backwards compatibility
// and never sent alongside a level, and a zero budget bought fewer thinking
// tokens on 3.7-flash than thinkingLevel "low" — while "low" cannot express
// what 3.5-flash still does, which is not think at all. One request shape per
// provider is the rule an A/B of narrators rests on, and a model-conditional
// split would spend it to make the shipped model worse. Should a generation
// refuse the field, the call fails closed: a 400 or a MAX_TOKENS truncation
// leaves the ride with the title it had.
type vertexThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type vertexGenerConfig struct {
	Temperature float64 `json:"temperature"`
	// A title is short; this bounds a runaway response rather than shaping
	// the answer.
	MaxOutputTokens int `json:"maxOutputTokens"`
	// The API is asked for JSON, but the validator is what enforces it —
	// see the package comment.
	ResponseMimeType string `json:"responseMimeType,omitempty"`

	ThinkingConfig *vertexThinkingConfig `json:"thinkingConfig,omitempty"`
}

type vertexResponse struct {
	Candidates []struct {
		Content      vertexContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
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
		Contents:          []vertexContent{{Role: roleUser, Parts: []vertexPart{{Text: prompt.User}}}},
		GenerationConfig: vertexGenerConfig{
			Temperature:      v.temperature(),
			MaxOutputTokens:  maxOutputTokens,
			ResponseMimeType: "application/json",
			ThinkingConfig:   &vertexThinkingConfig{ThinkingBudget: 0},
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

	// Logged before any verdict on the candidate, because the calls where the
	// ceiling mattered — a truncation, an empty candidate — are exactly the
	// ones whose token split answers why.
	if v.Logger != nil {
		finish := ""
		if len(decoded.Candidates) > 0 {
			finish = decoded.Candidates[0].FinishReason
		}

		v.Logger.Info("vertex usage",
			"model", v.model(),
			"prompt_tokens", decoded.UsageMetadata.PromptTokenCount,
			"thought_tokens", decoded.UsageMetadata.ThoughtsTokenCount,
			"output_tokens", decoded.UsageMetadata.CandidatesTokenCount,
			"total_tokens", decoded.UsageMetadata.TotalTokenCount,
			"max_output_tokens", maxOutputTokens,
			"finish_reason", finish)
	}

	if decoded.PromptFeedback.BlockReason != "" {
		return "", fmt.Errorf("naming: vertex: prompt blocked (%s)", decoded.PromptFeedback.BlockReason)
	}

	var text strings.Builder

	// One candidate is requested and the first is used; a provider that
	// returns more is not a reason to concatenate them into nonsense.
	if len(decoded.Candidates) > 0 {
		// The finish reason before the text: a candidate cut off at the
		// ceiling may hold half an object or nothing, and "no title" or
		// "not JSON" would name the wrong cause.
		if decoded.Candidates[0].FinishReason == vertexFinishMaxTokens {
			return "", truncatedError("vertex")
		}

		for _, part := range decoded.Candidates[0].Content.Parts {
			text.WriteString(part.Text)
		}
	}

	if text.Len() == 0 {
		return "", ErrNoTitle
	}

	return text.String(), nil
}

// vertexFinishMaxTokens is Vertex's finish reason for a candidate cut off at
// maxOutputTokens.
const vertexFinishMaxTokens = "MAX_TOKENS"

// truncatedError names a response cut off at the output ceiling, in the same
// words for every provider: the A/B's diagnostic is the log line, and one
// failure must not read as three causes across three narrators.
func truncatedError(provider string) error {
	return fmt.Errorf("naming: %s: response truncated at max_tokens (%d); the title did not fit",
		provider, maxOutputTokens)
}

// Message roles every provider's dialect spells the same way.
const (
	roleSystem = "system"
	roleUser   = "user"
)

// DefaultTemperature is the sampling temperature the spec asks for.
const DefaultTemperature = 0.9

// maxOutputTokens bounds a response that should be one short JSON object.
//
// The bound is sized for Vertex, where thinking tokens are billed inside it
// and Gemini 3.x cannot be stopped from thinking: a budget of zero buys a
// best-effort minimum of roughly 250 to 450 tokens, and a title that has to
// fit in what is left of a 256-token ceiling arrives truncated at MAX_TOKENS.
// A thousand leaves room for the deliberation and the answer both.
//
// The Anthropic and OpenRouter providers share the constant, where it stays
// what it reads as: a bound on a runaway response.
const maxOutputTokens = 1024

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
