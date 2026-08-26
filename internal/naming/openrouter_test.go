package naming

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The OpenRouter provider satisfies the interface the naming layer defines.
var _ Provider = (*OpenRouter)(nil)

// A completion round trip in the chat-completions dialect: the key as a
// bearer token, the attribution headers, the system and user roles, the
// pinned model and max_tokens in the body, and the first choice's text back —
// which the validator then accepts under the same contract as every other
// provider.
func TestOpenRouterComplete(t *testing.T) {
	t.Parallel()

	var (
		gotAuth, gotReferer, gotTitle, gotPath string
		gotBody                                map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		gotPath = r.URL.Path

		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&gotBody)

		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant",`+
			`"content":"{\"title\":\"Musterrunde\",\"language\":\"de\"}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	provider := &OpenRouter{Client: server.Client(), APIKey: "test-key", BaseURL: server.URL + "/api/v1"}

	raw, err := provider.Complete(t.Context(), Prompt{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, err := NewValidator(nil).ParseAndValidate(raw); err != nil {
		t.Fatalf("ParseAndValidate: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}

	if gotReferer == "" || gotTitle == "" {
		t.Errorf("attribution headers missing: referer %q, title %q", gotReferer, gotTitle)
	}

	if gotPath != "/api/v1/chat/completions" {
		t.Errorf("path = %q, want the chat-completions route under the base URL", gotPath)
	}

	if gotBody["model"] != DefaultOpenRouterModel {
		t.Errorf("model = %v, want the pinned %q", gotBody["model"], DefaultOpenRouterModel)
	}

	if _, ok := gotBody["max_tokens"]; !ok {
		t.Errorf("request is missing max_tokens: %v", gotBody)
	}

	if _, ok := gotBody["response_format"]; ok {
		t.Errorf("a provider-side JSON mode was sent; the validator is the contract: %v", gotBody)
	}

	messages, _ := gotBody["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want a system and a user message", gotBody["messages"])
	}

	first, _ := messages[0].(map[string]any)
	second, _ := messages[1].(map[string]any)

	if first["role"] != "system" || first["content"] != "sys" ||
		second["role"] != "user" || second["content"] != "usr" {
		t.Errorf("messages = %v", messages)
	}
}

func TestOpenRouterRequiresAKey(t *testing.T) {
	t.Parallel()

	if _, err := (&OpenRouter{}).Complete(t.Context(), Prompt{}); err == nil {
		t.Error("Complete with no key = nil error, want error")
	}
}

// A non-200 must not quote the body back: it can echo the request, and the
// request carries the prompt.
func TestOpenRouterErrorHidesTheBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt was: SECRET-PROMPT-TEXT"}}`)
	}))
	defer server.Close()

	provider := &OpenRouter{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

	_, err := provider.Complete(t.Context(), Prompt{})
	if err == nil {
		t.Fatal("want an error")
	}

	if strings.Contains(err.Error(), "SECRET-PROMPT-TEXT") {
		t.Errorf("the error echoed the response body: %v", err)
	}

	if !strings.Contains(err.Error(), "402") {
		t.Errorf("the error does not report the status: %v", err)
	}
}

// A response cut off at max_tokens is reported as that, not as "not JSON".
func TestOpenRouterReportsTruncation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"title\":\"Mus"},"finish_reason":"length"}]}`)
	}))
	defer server.Close()

	provider := &OpenRouter{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

	_, err := provider.Complete(t.Context(), Prompt{})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v, want the truncation named", err)
	}
}

// No choices, or an empty one, is no title rather than a decode error.
func TestOpenRouterEmptyChoices(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{"choices":[]}`, `{"choices":[{"message":{"content":""}}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))

		provider := &OpenRouter{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

		_, err := provider.Complete(t.Context(), Prompt{})
		server.Close()

		if err == nil || !strings.Contains(err.Error(), ErrNoTitle.Error()) {
			t.Errorf("body %s: err = %v, want ErrNoTitle", body, err)
		}
	}
}

func TestOpenRouterDefaults(t *testing.T) {
	t.Parallel()

	provider := &OpenRouter{}

	if got := provider.endpoint(); got != DefaultOpenRouterBaseURL+"/chat/completions" {
		t.Errorf("endpoint = %q", got)
	}

	if got := (&OpenRouter{BaseURL: "https://gateway.example/v1/"}).endpoint(); got != "https://gateway.example/v1/chat/completions" {
		t.Errorf("endpoint with a base URL = %q", got)
	}

	if name := provider.Name(); !strings.Contains(name, DefaultOpenRouterModel) {
		t.Errorf("Name() = %q", name)
	}

	if name := (&OpenRouter{Model: "google/gemini-3.7-flash"}).Name(); name != "openrouter/google/gemini-3.7-flash" {
		t.Errorf("Name() with a model = %q", name)
	}

	if provider.temperature() != DefaultTemperature || (&OpenRouter{Temperature: 0.3}).temperature() != 0.3 {
		t.Error("temperature default or override wrong")
	}

	if provider.client().Timeout == 0 {
		t.Error("the default client has no timeout")
	}
}

// A cancelled context stops the call.
func TestOpenRouterContextCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	provider := &OpenRouter{Client: server.Client(), APIKey: "k", BaseURL: server.URL}

	if _, err := provider.Complete(ctx, Prompt{}); err == nil {
		t.Error("Complete with a cancelled context = nil error")
	}
}
