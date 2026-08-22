package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Environment variables for the naming layer.
//
// LLM_API_KEY is deliberately not among the required set. The default provider
// is Gemini through Vertex AI, which authenticates with the runtime service
// account's ambient credentials and has no key at all — the key exists only for
// the Anthropic alternative, and is required only when that is selected.
const (
	EnvLLMProvider    = "LLM_PROVIDER"
	EnvLLMModel       = "LLM_MODEL"
	EnvLLMAPIKey      = "LLM_API_KEY" //nolint:gosec // the name of a variable, not a value
	EnvVertexProject  = "VERTEX_PROJECT"
	EnvVertexLocation = "VERTEX_LOCATION"
	EnvBannedWords    = "BANNED_WORDS"
	EnvMachineTitles  = "MACHINE_TITLE_PATTERNS"
)

// Provider selects the LLM backend.
type Provider string

const (
	// ProviderVertex is Gemini through Vertex AI: keyless, using the runtime
	// service account. This is the default and the zero value's meaning.
	ProviderVertex Provider = "gemini"

	// ProviderAnthropic is the Haiku-class alternative, which needs a key.
	ProviderAnthropic Provider = "anthropic"
)

// DefaultVertexLocation is where the Vertex call goes.
//
// europe-west3, the same region as Firestore, Cloud Run and the rest of this
// deployment — and confirmed by reading the publisher-model metadata there
// rather than assumed, because model availability is regional and does not
// follow the documentation's model index.
//
// "global" is also accepted, and reaches models that no European region
// serves, at the cost of routing the request wherever there is capacity. The
// trade-off and the probe are in README.md.
const DefaultVertexLocation = "europe-west3"

// vertexLocationPattern bounds VERTEX_LOCATION to the shape of a GCP location.
//
// The value is interpolated into the request host, so "evil.example/x" would
// produce https://evil.example/x-aiplatform.googleapis.com and carry the
// runtime account's credentials to a host this deployment never meant to
// reach. The value is operator-supplied, so this is defense in depth rather
// than a hole being closed — and it costs a startup error instead of a failure
// on the first ride worth naming.
var vertexLocationPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,38}[a-z0-9]$`)

// LLM is the naming layer's configuration.
type LLM struct {
	// Provider selects the backend. Empty means [ProviderVertex].
	Provider Provider

	// Model overrides the provider's shipped model ID. Empty means the
	// provider default, which is pinned in the naming package next to the
	// documentation reference it was verified against.
	Model string

	// APIKey authenticates Anthropic. Empty for Vertex, and never read there.
	APIKey string

	// VertexProject and VertexLocation address the Vertex endpoint.
	// VertexProject falls back to the Firestore project, which is the same
	// project in every deployment this service has.
	VertexProject  string
	VertexLocation string

	// BannedWords are rejected in a generated title. Empty here means the
	// environment named none; the shipped list is substituted where the
	// validator is built, the same way an empty MachineTitlePatterns falls
	// back to the shipped set. This package reports what the environment
	// said and resolves no defaults from the naming layer.
	BannedWords []string

	// MachineTitlePatterns are the titles another tool wrote that may be
	// replaced. Empty means the shipped set, which is Xert's pattern.
	MachineTitlePatterns []string
}

// loadLLM reads the naming configuration, appending to errs rather than
// returning early, so one pass reports everything wrong at once.
func loadLLM(getenv func(string) string, firestoreProject string, errs *[]error) LLM {
	llm := LLM{
		Provider:       ProviderVertex,
		Model:          strings.TrimSpace(getenv(EnvLLMModel)),
		APIKey:         strings.TrimSpace(getenv(EnvLLMAPIKey)),
		VertexProject:  strings.TrimSpace(getenv(EnvVertexProject)),
		VertexLocation: strings.TrimSpace(getenv(EnvVertexLocation)),
	}

	switch raw := strings.ToLower(strings.TrimSpace(getenv(EnvLLMProvider))); raw {
	case "", string(ProviderVertex):
		llm.Provider = ProviderVertex
	case string(ProviderAnthropic):
		llm.Provider = ProviderAnthropic
	default:
		*errs = append(*errs, fmt.Errorf("config: %s must be %q or %q, got %q",
			EnvLLMProvider, ProviderVertex, ProviderAnthropic, raw))
	}

	if llm.VertexProject == "" {
		llm.VertexProject = firestoreProject
	}

	if llm.VertexLocation == "" {
		llm.VertexLocation = DefaultVertexLocation
	}

	// Fail closed, and fail at startup. A key missing here would otherwise
	// surface as an authentication error on the first ride worth naming.
	if llm.Provider == ProviderAnthropic && llm.APIKey == "" {
		*errs = append(*errs, errors.New(
			"config: "+EnvLLMAPIKey+" is required when "+EnvLLMProvider+"=anthropic"))
	}

	// No startup error for a missing Vertex project. With FIRESTORE_PROJECT
	// unset the service runs on the in-memory store — the documented local
	// mode — and requiring a GCP project there would make the service refuse
	// to start on a laptop. The Vertex provider refuses the call instead, at
	// the point where the project is actually needed.

	if !vertexLocationPattern.MatchString(llm.VertexLocation) {
		*errs = append(*errs, fmt.Errorf(
			"config: %s must be a GCP location such as europe-west3 or global, got %q",
			EnvVertexLocation, llm.VertexLocation))
	}

	llm.BannedWords = splitList(getenv(EnvBannedWords))

	// Newline-separated, not comma-separated: these are regular expressions,
	// and a comma is a perfectly ordinary character inside one.
	llm.MachineTitlePatterns = splitLines(getenv(EnvMachineTitles))

	return llm
}

// splitList parses a comma-separated list, dropping blanks.
func splitList(raw string) []string {
	var out []string

	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// splitLines parses a newline-separated list, dropping blanks.
func splitLines(raw string) []string {
	var out []string

	for _, item := range strings.Split(raw, "\n") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}
