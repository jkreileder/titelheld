package naming

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic identifiers and credentials. None of them names anything real.
const (
	wifTestRule          = "fdrl_test0000000000000000"
	wifTestOrganization  = "00000000-1111-4222-8333-444444444444"
	wifTestAccount       = "svac_test0000000000000000"
	wifTestWorkspace     = "wrkspc_test000000000000"
	wifTestIdentityToken = "eyJ-fake-identity-token.payload.signature"
	wifTestAccessToken   = "sk-ant-oat01-fake-access-token"
)

// wifMessagesResponse mirrors the Messages API's shape: a thinking block
// before the text block, thinking tokens under output_tokens_details, and
// stop_details.
const wifMessagesResponse = `{
  "id": "msg_test",
  "type": "message",
  "role": "assistant",
  "model": "claude-fable-5-1",
  "content": [
    {"type": "thinking", "thinking": "a short deliberation", "signature": "sig"},
    {"type": "text", "text": "{\"title\": \"Testrunde am Abend\", \"language\": \"de\"}"}
  ],
  "stop_reason": "end_turn",
  "stop_details": null,
  "usage": {"input_tokens": 812, "output_tokens": 41, "output_tokens_details": {"thinking_tokens": 17}}
}`

const wifExchangeResponse = `{"access_token": "` + wifTestAccessToken + `", "token_type": "Bearer", ` +
	`"expires_in": 3600, "scope": "workspace:developer"}`

// wifServer fakes both Anthropic routes and records what reached them.
type wifServer struct {
	*httptest.Server

	exchanges   atomic.Int64
	completions atomic.Int64

	mu            sync.Mutex
	exchangeBody  map[string]any
	messagesBody  map[string]any
	authorization string
	version       string

	exchangeStatus int
	exchangeReply  string
	messagesReply  string
}

func newWIFServer(t *testing.T) *wifServer {
	t.Helper()

	fake := &wifServer{exchangeStatus: http.StatusOK, exchangeReply: wifExchangeResponse, messagesReply: wifMessagesResponse}

	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)

		fake.mu.Lock()
		defer fake.mu.Unlock()

		switch r.URL.Path {
		case "/v1/oauth/token":
			fake.exchanges.Add(1)
			fake.exchangeBody = body

			w.WriteHeader(fake.exchangeStatus)
			_, _ = io.WriteString(w, fake.exchangeReply)
		case "/v1/messages":
			fake.completions.Add(1)
			fake.messagesBody = body
			fake.authorization = r.Header.Get("Authorization")
			fake.version = r.Header.Get("Anthropic-Version")

			_, _ = io.WriteString(w, fake.messagesReply)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.Close)

	return fake
}

// wifIdentity records the audiences it was asked for.
type wifIdentity struct {
	mu        sync.Mutex
	audiences []string
}

func (i *wifIdentity) token(_ context.Context, audience string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.audiences = append(i.audiences, audience)

	return wifTestIdentityToken, nil
}

func newWIFProvider(fake *wifServer, identity *wifIdentity) *AnthropicWIF {
	return &AnthropicWIF{
		Client:           fake.Client(),
		BaseURL:          fake.URL,
		IdentityToken:    identity.token,
		FederationRuleID: wifTestRule,
		OrganizationID:   wifTestOrganization,
		ServiceAccountID: wifTestAccount,
		WorkspaceID:      wifTestWorkspace,
	}
}

// A cold call exchanges, then completes: the exchange carries the identity
// token and the four identifiers, the completion carries the access token as
// a bearer and exactly the documented body — effort low, no temperature, no
// thinking — and the text block past the thinking block is returned.
func TestAnthropicWIFComplete(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	identity := &wifIdentity{}
	provider := newWIFProvider(fake, identity)
	provider.Audience = "https://audience.example"

	raw, err := provider.Complete(t.Context(), Prompt{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	title, _, err := NewValidator(nil).ParseAndValidate(raw)
	if err != nil || title.Text != "Testrunde am Abend" {
		t.Fatalf("ParseAndValidate = %+v, %v", title, err)
	}

	if len(identity.audiences) != 1 || identity.audiences[0] != "https://audience.example" {
		t.Errorf("identity token audiences = %v, want the configured audience once", identity.audiences)
	}

	wantExchange := map[string]any{
		"grant_type":         jwtBearerGrant,
		"assertion":          wifTestIdentityToken,
		"federation_rule_id": wifTestRule,
		"organization_id":    wifTestOrganization,
		"service_account_id": wifTestAccount,
		"workspace_id":       wifTestWorkspace,
	}
	if len(fake.exchangeBody) != len(wantExchange) {
		t.Errorf("exchange body = %v, want exactly %v", fake.exchangeBody, wantExchange)
	}

	for key, want := range wantExchange {
		if fake.exchangeBody[key] != want {
			t.Errorf("exchange %s = %v, want %v", key, fake.exchangeBody[key], want)
		}
	}

	if fake.authorization != "Bearer "+wifTestAccessToken {
		t.Errorf("Authorization = %q, want the exchanged bearer", fake.authorization)
	}

	if fake.version != anthropicVersion {
		t.Errorf("anthropic-version = %q", fake.version)
	}

	body := fake.messagesBody

	wantKeys := []string{"model", "max_tokens", "system", "messages", "output_config"}
	if len(body) != len(wantKeys) {
		t.Errorf("messages body keys = %v, want exactly %v", body, wantKeys)
	}

	for _, absent := range []string{"temperature", "thinking", "tools", "inference_geo"} {
		if _, ok := body[absent]; ok {
			t.Errorf("messages body carries %q: %v", absent, body)
		}
	}

	if body["model"] != DefaultAnthropicWIFModel || maxOutputTokens != 1024 ||
		body["max_tokens"] != float64(1024) || body["system"] != "sys" {
		t.Errorf("messages body = %v", body)
	}

	output, _ := body["output_config"].(map[string]any)
	if output["effort"] != "low" || len(output) != 1 {
		t.Errorf("output_config = %v, want effort low and nothing else", body["output_config"])
	}

	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want one user message", body["messages"])
	}

	if message, _ := messages[0].(map[string]any); message["role"] != "user" || message["content"] != "usr" {
		t.Errorf("message = %v", messages[0])
	}
}

// Without a workspace the exchange sends no workspace_id at all.
func TestAnthropicWIFOmitsAnUnsetWorkspace(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	provider := newWIFProvider(fake, &wifIdentity{})
	provider.WorkspaceID = ""

	if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if _, ok := fake.exchangeBody["workspace_id"]; ok {
		t.Errorf("exchange body = %v, want no workspace_id", fake.exchangeBody)
	}

	if provider.audience() != DefaultAnthropicWIFAudience {
		t.Errorf("audience = %q, want the default", provider.audience())
	}
}

// The federation's opaque denial is a provider error that names the status
// and never the assertion; no completion is attempted, and nothing is cached.
func TestAnthropicWIFExchangeDenied(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	fake.exchangeStatus = http.StatusUnauthorized
	fake.exchangeReply = `{"type":"error","error":{"type":"authentication_error","message":"Authentication failed"}}`
	provider := newWIFProvider(fake, &wifIdentity{})

	_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
	if err == nil {
		t.Fatal("Complete after a 401 exchange = nil error")
	}

	if !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), wifTestIdentityToken) ||
		strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("error = %q, want the status and neither the assertion nor the body", err)
	}

	if fake.completions.Load() != 0 {
		t.Errorf("completions = %d after a failed exchange, want 0", fake.completions.Load())
	}

	// A failure caches nothing: the next call exchanges again.
	_, _ = provider.Complete(t.Context(), Prompt{System: "s", User: "u"})

	if fake.exchanges.Load() != 2 {
		t.Errorf("exchanges = %d, want a second attempt after a failure", fake.exchanges.Load())
	}
}

// A 200 the provider cannot use is an error too, and caches nothing.
func TestAnthropicWIFExchangeMalformed(t *testing.T) {
	t.Parallel()

	for _, reply := range []string{
		`not json`,
		`{"access_token": "", "token_type": "Bearer", "expires_in": 3600}`,
		`{"access_token": "` + wifTestAccessToken + `", "token_type": "Bearer", "expires_in": 0}`,
		`{"access_token": "` + wifTestAccessToken + `", "token_type": "mac", "expires_in": 3600}`,
	} {
		fake := newWIFServer(t)
		fake.exchangeReply = reply
		provider := newWIFProvider(fake, &wifIdentity{})

		_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
		if err == nil {
			t.Errorf("exchange reply %q accepted", reply)

			continue
		}

		if strings.Contains(err.Error(), wifTestAccessToken) || strings.Contains(err.Error(), wifTestIdentityToken) {
			t.Errorf("error carries a token: %q", err)
		}

		if provider.token != "" {
			t.Errorf("reply %q cached a token", reply)
		}
	}
}

// Two completions inside the token's lifetime share one exchange, and each
// exchange is given a freshly minted identity token.
func TestAnthropicWIFReusesTheAccessToken(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	identity := &wifIdentity{}
	provider := newWIFProvider(fake, identity)

	for range 2 {
		if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}

	if fake.exchanges.Load() != 1 || fake.completions.Load() != 2 || len(identity.audiences) != 1 {
		t.Errorf("exchanges = %d, completions = %d, identity mints = %d; want 1, 2, 1",
			fake.exchanges.Load(), fake.completions.Load(), len(identity.audiences))
	}
}

// The cached token is replaced when fewer than 120 seconds remain, and only
// then.
func TestAnthropicWIFRefreshesBeforeExpiry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		remaining time.Duration
		exchanges int64
	}{
		{"200s left keeps the token", 200 * time.Second, 1},
		{"100s left refreshes", 100 * time.Second, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newWIFServer(t)
			identity := &wifIdentity{}
			provider := newWIFProvider(fake, identity)

			start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
			clock := start
			provider.now = func() time.Time { return clock }

			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			// The fixture's expires_in is 3600 seconds.
			clock = start.Add(3600*time.Second - tc.remaining)

			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			if got := fake.exchanges.Load(); got != tc.exchanges {
				t.Errorf("exchanges = %d, want %d", got, tc.exchanges)
			}

			if int64(len(identity.audiences)) != tc.exchanges {
				t.Errorf("identity mints = %d, want one per exchange (%d)", len(identity.audiences), tc.exchanges)
			}
		})
	}
}

// Concurrent callers from cold share one exchange.
func TestAnthropicWIFSingleFlight(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	provider := newWIFProvider(fake, &wifIdentity{})

	var (
		wg     sync.WaitGroup
		failed atomic.Int64
	)

	for range 8 {
		wg.Go(func() {
			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				failed.Add(1)
			}
		})
	}

	wg.Wait()

	if failed.Load() != 0 {
		t.Errorf("%d concurrent completions failed", failed.Load())
	}

	if fake.exchanges.Load() != 1 || fake.completions.Load() != 8 {
		t.Errorf("exchanges = %d, completions = %d; want 1 and 8", fake.exchanges.Load(), fake.completions.Load())
	}
}

// A refusal names its category, a truncation is the shared truncation error,
// and both are decided before the text is looked at.
func TestAnthropicWIFStopReasons(t *testing.T) {
	t.Parallel()

	text := `"content": [{"type": "text", "text": "{\"title\": \"Testrunde\", \"language\": \"de\"}"}], `

	for _, tc := range []struct {
		name  string
		reply string
		want  []string
	}{
		{
			"refusal", `{` + text + `"stop_reason": "refusal", "stop_details": {"type": "refusal", "category": "cyber"}}`,
			[]string{"naming: anthropic-wif: refused (cyber)"},
		},
		{
			"refusal without details", `{` + text + `"stop_reason": "refusal"}`,
			[]string{"naming: anthropic-wif: refused ()"},
		},
		{
			"max_tokens", `{` + text + `"stop_reason": "max_tokens"}`,
			[]string{truncatedError("anthropic-wif").Error(), "truncated"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newWIFServer(t)
			fake.messagesReply = tc.reply

			_, err := newWIFProvider(fake, &wifIdentity{}).Complete(t.Context(), Prompt{System: "s", User: "u"})
			if err == nil {
				t.Fatal("Complete = nil error")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
		})
	}
}

// Thinking blocks are skipped; a response with no text block is no title.
func TestAnthropicWIFTextExtraction(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	provider := newWIFProvider(fake, &wifIdentity{})

	raw, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if strings.Contains(raw, "deliberation") || !strings.HasPrefix(raw, `{"title"`) {
		t.Errorf("raw = %q, want the text block alone", raw)
	}

	fake.mu.Lock()
	fake.messagesReply = `{"content": [{"type": "thinking", "thinking": "only this"}], "stop_reason": "end_turn"}`
	fake.mu.Unlock()

	if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); !errors.Is(err, ErrNoTitle) {
		t.Errorf("thinking-only response = %v, want ErrNoTitle", err)
	}
}

// A non-200 completion names the status and never quotes the body.
func TestAnthropicWIFCompletionStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			_, _ = io.WriteString(w, wifExchangeResponse)

			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"prompt was: SECRET-PROMPT-TEXT"}`)
	}))
	defer server.Close()

	provider := &AnthropicWIF{
		Client: server.Client(), BaseURL: server.URL, IdentityToken: (&wifIdentity{}).token,
		FederationRuleID: wifTestRule, OrganizationID: wifTestOrganization, ServiceAccountID: wifTestAccount,
	}

	_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error = %v, want the status without the body", err)
	}
}

// Neither token reaches a log line or an error, on the success path or on any
// failure path; the usage and exchange lines carry what they promise.
func TestAnthropicWIFNeverLogsAToken(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var errs []string

	fake := newWIFServer(t)
	provider := newWIFProvider(fake, &wifIdentity{})
	provider.Logger = logger

	if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for _, reply := range []string{
		`{"content": [], "stop_reason": "refusal", "stop_details": {"category": "cyber"}}`,
		`{"content": [], "stop_reason": "max_tokens"}`,
		`{"content": [], "stop_reason": "end_turn"}`,
		`not json`,
	} {
		fake.mu.Lock()
		fake.messagesReply = reply
		fake.mu.Unlock()

		if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
			errs = append(errs, err.Error())
		}
	}

	denied := newWIFServer(t)
	denied.exchangeStatus = http.StatusUnauthorized
	deniedProvider := newWIFProvider(denied, &wifIdentity{})
	deniedProvider.Logger = logger

	if _, err := deniedProvider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) != 5 {
		t.Fatalf("errors = %v, want one per failure path", errs)
	}

	output := logs.String()

	for _, secret := range []string{wifTestAccessToken, wifTestIdentityToken, "oat01"} {
		if strings.Contains(output, secret) {
			t.Errorf("logs carry %q:\n%s", secret, output)
		}

		for _, message := range errs {
			if strings.Contains(message, secret) {
				t.Errorf("error carries %q: %s", secret, message)
			}
		}
	}

	for _, want := range []string{
		`"msg":"anthropic exchange"`, `"expires_in":3600`, `"scope":"workspace:developer"`,
		`"msg":"anthropic usage"`, `"model":"claude-fable-5-1"`, `"input_tokens":812`,
		`"output_tokens":41`, `"thinking_tokens":17`, `"stop_reason":"end_turn"`, `"latency_ms":`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("logs lack %s:\n%s", want, output)
		}
	}
}

// Missing configuration is refused before anything is sent.
func TestAnthropicWIFRejectsMissingConfiguration(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)

	noSource := newWIFProvider(fake, &wifIdentity{})
	noSource.IdentityToken = nil

	noRule := newWIFProvider(fake, &wifIdentity{})
	noRule.FederationRuleID = ""

	empty := newWIFProvider(fake, &wifIdentity{})
	empty.IdentityToken = func(context.Context, string) (string, error) { return "", nil }

	failing := newWIFProvider(fake, &wifIdentity{})
	failing.IdentityToken = func(context.Context, string) (string, error) {
		return "", errors.New("metadata identity: unexpected status 404")
	}

	for name, provider := range map[string]*AnthropicWIF{
		"no identity source": noSource, "no rule": noRule, "empty identity token": empty, "failing source": failing,
	} {
		if _, err := provider.Complete(t.Context(), Prompt{}); err == nil {
			t.Errorf("%s: Complete = nil error", name)
		}
	}

	if fake.exchanges.Load() != 0 {
		t.Errorf("exchanges = %d, want none sent", fake.exchanges.Load())
	}
}

func TestAnthropicWIFNameAndDefaults(t *testing.T) {
	t.Parallel()

	if name := (&AnthropicWIF{}).Name(); name != "anthropic-wif/claude-fable-5-1 effort=low" {
		t.Errorf("Name() = %q", name)
	}

	if name := (&AnthropicWIF{Model: "claude-other"}).Name(); name != "anthropic-wif/claude-other effort=low" {
		t.Errorf("Name() = %q", name)
	}

	if client := (&AnthropicWIF{}).client(); client.Timeout != defaultTimeout {
		t.Errorf("default client timeout = %v, want %v", client.Timeout, defaultTimeout)
	}

	if got := (&AnthropicWIF{}).url("/v1/messages"); got != "https://api.anthropic.com/v1/messages" {
		t.Errorf("default endpoint = %q", got)
	}

	if got := capScope(strings.Repeat("x", 300)); len(got) != 128 {
		t.Errorf("capScope length = %d, want 128", len(got))
	}
}

var _ Provider = (*AnthropicWIF)(nil)
