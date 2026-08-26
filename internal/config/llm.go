package config

import (
	"cmp"
	"fmt"
	"regexp"
	"strings"
)

// Environment variables for the naming layer.
//
// LLM_API_KEY is deliberately not among the required set. The default provider
// is Gemini through Vertex AI, which authenticates with the runtime service
// account's ambient credentials and has no key at all — the key exists only for
// the two keyed alternatives, Anthropic and OpenRouter, and is required only
// when one of them is selected.
const (
	EnvLLMProvider    = "LLM_PROVIDER"
	EnvLLMModel       = "LLM_MODEL"
	EnvLLMAPIKey      = "LLM_API_KEY" //nolint:gosec // the name of a variable, not a value
	EnvLLMBaseURL     = "LLM_BASE_URL"
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

	// ProviderOpenRouter is an OpenAI-compatible chat-completions client
	// against a configurable base URL: one key, many narrators, for an A/B
	// of models on the same queued ride. Needs a key.
	ProviderOpenRouter Provider = "openrouter"
)

// openRouterBaseURLPattern bounds LLM_BASE_URL to an https origin with an
// optional path, on the same reasoning as [vertexLocationPattern]: the value
// names the host the API key is sent to in a header, so a plain-http or
// malformed value would carry the key somewhere it was never meant to go —
// and a startup error is cheaper than a leak on the first ride worth naming.
var openRouterBaseURLPattern = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// Keyed reports whether the provider authenticates with LLM_API_KEY.
func (p Provider) Keyed() bool {
	return p == ProviderAnthropic || p == ProviderOpenRouter
}

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

	// APIKey authenticates Anthropic or OpenRouter. Empty for Vertex, where
	// the variable is not even looked up.
	APIKey string

	// BaseURL is the API root the OpenRouter provider calls. Empty means
	// OpenRouter's own; the naming package holds the default. Read and
	// validated only when that provider is selected.
	BaseURL string

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
		VertexProject:  strings.TrimSpace(getenv(EnvVertexProject)),
		VertexLocation: strings.TrimSpace(getenv(EnvVertexLocation)),
	}

	switch raw := strings.ToLower(strings.TrimSpace(getenv(EnvLLMProvider))); raw {
	case "", string(ProviderVertex):
		llm.Provider = ProviderVertex
	case string(ProviderAnthropic):
		llm.Provider = ProviderAnthropic
	case string(ProviderOpenRouter):
		llm.Provider = ProviderOpenRouter
	default:
		*errs = append(*errs, fmt.Errorf("config: %s must be %q, %q or %q, got %q",
			EnvLLMProvider, ProviderVertex, ProviderAnthropic, ProviderOpenRouter, raw))
	}

	llm.VertexProject = cmp.Or(llm.VertexProject, firestoreProject)
	llm.VertexLocation = cmp.Or(llm.VertexLocation, DefaultVertexLocation)

	// The key and the base URL are read only for a provider that uses them.
	// "The keyless default reads no key" is then a property of the loader
	// rather than of whatever the environment happened to hold, and a test
	// can count the lookups.
	if llm.Provider.Keyed() {
		llm.APIKey = strings.TrimSpace(getenv(EnvLLMAPIKey))

		// Fail closed, and fail at startup. A key missing here would
		// otherwise surface as an authentication error on the first ride
		// worth naming.
		if llm.APIKey == "" {
			*errs = append(*errs, fmt.Errorf(
				"config: %s is required when %s=%s, and it is unset",
				EnvLLMAPIKey, EnvLLMProvider, llm.Provider))
		}
	}

	if llm.Provider == ProviderOpenRouter {
		llm.BaseURL = strings.TrimRight(strings.TrimSpace(getenv(EnvLLMBaseURL)), "/")

		if llm.BaseURL != "" && !openRouterBaseURLPattern.MatchString(llm.BaseURL) {
			*errs = append(*errs, fmt.Errorf(
				"config: %s must be an https origin such as https://openrouter.ai/api/v1, got %q",
				EnvLLMBaseURL, llm.BaseURL))
		}
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

	for item := range strings.SplitSeq(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}

// splitLines parses a newline-separated list, dropping blanks.
func splitLines(raw string) []string {
	var out []string

	for item := range strings.SplitSeq(raw, "\n") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}
