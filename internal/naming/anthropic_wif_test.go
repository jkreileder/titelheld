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
	"strconv"
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

	// rotateTokens makes exchange n issue wifRotatedToken(n) rather than the
	// fixed reply, so a test can tell a rejected token from its replacement.
	rotateTokens bool

	// messagesStatus decides the status of completion call (counted from 1)
	// presented with authorization. Nil answers 200. A non-200 reply carries a
	// body that echoes a prompt marker, which must never surface.
	messagesStatus func(call int64, authorization string) int

	// messagesGate runs before the handler takes mu, so it may block a
	// completion until others have arrived.
	messagesGate func(authorization string)
}

// wifRotatedToken is the access token a rotating fake issues on exchange n.
// It keeps the fixed token's prefix, so a search for "oat01" finds any of them.
func wifRotatedToken(n int64) string {
	return wifTestAccessToken + "-" + strconv.FormatInt(n, 10)
}

// wifRejectedBody is what a fake returns with a non-200 completion.
const wifRejectedBody = `{"type":"error","error":{"message":"prompt was: SECRET-PROMPT-TEXT"}}`

func newWIFServer(t *testing.T) *wifServer {
	t.Helper()

	fake := &wifServer{exchangeStatus: http.StatusOK, exchangeReply: wifExchangeResponse, messagesReply: wifMessagesResponse}

	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any

		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)

		if r.URL.Path == "/v1/messages" && fake.messagesGate != nil {
			fake.messagesGate(r.Header.Get("Authorization"))
		}

		fake.mu.Lock()
		defer fake.mu.Unlock()

		switch r.URL.Path {
		case "/v1/oauth/token":
			n := fake.exchanges.Add(1)
			fake.exchangeBody = body

			reply := fake.exchangeReply
			if fake.rotateTokens {
				reply = `{"access_token": "` + wifRotatedToken(n) + `", "token_type": "Bearer", "expires_in": 3600}`
			}

			w.WriteHeader(fake.exchangeStatus)
			_, _ = io.WriteString(w, reply)
		case "/v1/messages":
			call := fake.completions.Add(1)
			fake.messagesBody = body
			fake.authorization = r.Header.Get("Authorization")
			fake.version = r.Header.Get("Anthropic-Version")

			if fake.messagesStatus != nil {
				if status := fake.messagesStatus(call, fake.authorization); status != http.StatusOK {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, wifRejectedBody)

					return
				}
			}

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
	logger, logs := wifLogger()
	provider := newWIFProvider(fake, &wifIdentity{})
	provider.Logger = logger

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
		t.Errorf("exchanges = %d, want one per call and no retry of the exchange", fake.exchanges.Load())
	}

	// A denied exchange is not a rejected access token: there is nothing to
	// recover from, and no recovery is logged.
	if lines := wifRecoveryLines(t, logs); len(lines) != 0 {
		t.Errorf("recovery lines = %v after a denied exchange, want none", lines)
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

// wifStatuses answers completion call n with statuses[n-1], and 200 past the
// end of the list.
func wifStatuses(statuses ...int) func(int64, string) int {
	return func(call int64, _ string) int {
		if call <= int64(len(statuses)) {
			return statuses[call-1]
		}

		return http.StatusOK
	}
}

// wifRecoveryLines returns the decoded "re-exchanging" lines in a JSON log.
func wifRecoveryLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for raw := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}

		if line["msg"] == "anthropic access token rejected; re-exchanging" {
			lines = append(lines, line)
		}
	}

	return lines
}

func wifLogger() (*slog.Logger, *bytes.Buffer) {
	var logs bytes.Buffer

	return slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), &logs
}

// wifWarm runs one successful completion, which leaves the first exchanged
// token in the cache.
func wifWarm(t *testing.T, provider *AnthropicWIF) {
	t.Helper()

	if _, err := provider.Complete(t.Context(), Prompt{System: "warm", User: "warm"}); err != nil {
		t.Fatalf("warming Complete: %v", err)
	}
}

// A cached token rejected with 401 is dropped, a fresh one is exchanged and
// the same request is resent with it; the title comes back and one warning,
// carrying the status and nothing else, records the recovery.
func TestAnthropicWIFRecoversFromARejectedToken(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	fake.rotateTokens = true
	fake.messagesStatus = wifStatuses(http.StatusOK, http.StatusUnauthorized)

	logger, logs := wifLogger()
	provider := newWIFProvider(fake, &wifIdentity{})
	provider.Logger = logger

	wifWarm(t, provider)

	raw, err := provider.Complete(t.Context(), Prompt{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete after one 401 = %v, want the retried title", err)
	}

	if title, _, err := NewValidator(nil).ParseAndValidate(raw); err != nil || title.Text != "Testrunde am Abend" {
		t.Errorf("ParseAndValidate = %+v, %v", title, err)
	}

	if fake.exchanges.Load() != 2 || fake.completions.Load() != 3 {
		t.Errorf("exchanges = %d, completions = %d; want 2 and 3", fake.exchanges.Load(), fake.completions.Load())
	}

	if fake.authorization != "Bearer "+wifRotatedToken(2) {
		t.Errorf("retry Authorization = %q, want the second exchange's token", fake.authorization)
	}

	if retried := fake.messagesBody; retried["system"] != "sys" || retried["model"] != DefaultAnthropicWIFModel {
		t.Errorf("retried body = %v, want the original request", retried)
	}

	lines := wifRecoveryLines(t, logs)
	if len(lines) != 1 {
		t.Fatalf("recovery lines = %v, want exactly one:\n%s", lines, logs)
	}

	if len(lines[0]) != 4 || lines[0]["level"] != "WARN" || lines[0]["status"] != float64(http.StatusUnauthorized) {
		t.Errorf("recovery line = %v, want time, level WARN, msg and status 401 alone", lines[0])
	}

	// The replacement is cached: the next completion neither exchanges nor
	// warns.
	if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if fake.exchanges.Load() != 2 || len(wifRecoveryLines(t, logs)) != 1 {
		t.Errorf("exchanges = %d after a clean completion, want 2 and no second warning", fake.exchanges.Load())
	}
}

// A cached token rejected, a fresh exchange, and the fresh token rejected too
// ends the call with the distinct error and no third attempt, and leaves
// nothing cached: the next call exchanges for itself. A retry rejected for any
// other reason reports that status through the ordinary status error and
// keeps its token.
func TestAnthropicWIFRejectedTwice(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		second    int
		want      string
		exchanges int64
	}{
		{"401 twice", http.StatusUnauthorized,
			"naming: anthropic-wif: access token rejected twice (status 401 after a fresh exchange)", 3},
		{"401 then 500", http.StatusInternalServerError, "naming: anthropic-wif: unexpected status 500", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newWIFServer(t)
			fake.rotateTokens = true
			fake.messagesStatus = wifStatuses(http.StatusOK, http.StatusUnauthorized, tc.second)

			logger, logs := wifLogger()
			provider := newWIFProvider(fake, &wifIdentity{})
			provider.Logger = logger

			wifWarm(t, provider)

			_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
			if err == nil || err.Error() != tc.want {
				t.Errorf("error = %v, want exactly %q", err, tc.want)
			}

			if fake.exchanges.Load() != 2 || fake.completions.Load() != 3 {
				t.Errorf("exchanges = %d, completions = %d; want 2 and 3",
					fake.exchanges.Load(), fake.completions.Load())
			}

			if len(wifRecoveryLines(t, logs)) != 1 {
				t.Errorf("recovery lines = %d, want one per call:\n%s", len(wifRecoveryLines(t, logs)), logs)
			}

			// The token rejected twice is not cached; one rejected for
			// another reason is.
			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			if got := fake.exchanges.Load(); got != tc.exchanges {
				t.Errorf("exchanges after the next Complete = %d, want %d", got, tc.exchanges)
			}
		})
	}
}

// A token this call exchanged and had rejected on its first use was not
// revoked early: the call returns the ordinary status error with one send,
// one exchange and no warning, and the token is not cached.
func TestAnthropicWIFFreshTokenRejected(t *testing.T) {
	t.Parallel()

	fake := newWIFServer(t)
	fake.rotateTokens = true
	fake.messagesStatus = wifStatuses(http.StatusUnauthorized)

	logger, logs := wifLogger()
	provider := newWIFProvider(fake, &wifIdentity{})
	provider.Logger = logger

	_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
	if want := "naming: anthropic-wif: unexpected status 401"; err == nil || err.Error() != want {
		t.Errorf("error = %v, want exactly %q", err, want)
	}

	if fake.exchanges.Load() != 1 || fake.completions.Load() != 1 {
		t.Errorf("exchanges = %d, completions = %d; want 1 and 1", fake.exchanges.Load(), fake.completions.Load())
	}

	if lines := wifRecoveryLines(t, logs); len(lines) != 0 {
		t.Errorf("recovery lines = %v after a fresh token's 401, want none", lines)
	}

	if provider.token != "" {
		t.Error("the rejected token is still cached")
	}

	if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if fake.exchanges.Load() != 2 || fake.authorization != "Bearer "+wifRotatedToken(2) {
		t.Errorf("exchanges = %d, Authorization = %q; want a second exchange and its token",
			fake.exchanges.Load(), fake.authorization)
	}
}

// Any other non-200 from the Messages API returns the status error, leaves
// the cached token alone and warns of nothing; the next completion reuses it.
func TestAnthropicWIFOtherStatusesKeepTheToken(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()

			fake := newWIFServer(t)
			fake.rotateTokens = true
			fake.messagesStatus = wifStatuses(status)

			logger, logs := wifLogger()
			provider := newWIFProvider(fake, &wifIdentity{})
			provider.Logger = logger

			_, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"})
			if want := "naming: anthropic-wif: unexpected status " + strconv.Itoa(status); err == nil || err.Error() != want {
				t.Errorf("error = %v, want exactly %q", err, want)
			}

			if fake.completions.Load() != 1 {
				t.Errorf("completions = %d after a %d, want no retry", fake.completions.Load(), status)
			}

			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			if fake.exchanges.Load() != 1 || fake.authorization != "Bearer "+wifRotatedToken(1) {
				t.Errorf("exchanges = %d, Authorization = %q; want the first token reused",
					fake.exchanges.Load(), fake.authorization)
			}

			if lines := wifRecoveryLines(t, logs); len(lines) != 0 {
				t.Errorf("recovery lines = %v after a %d, want none", lines, status)
			}
		})
	}
}

// Concurrent completions whose first attempts all present the same cached
// token, and all have it rejected, finish without a failure and with a single
// re-exchange under this test's ordering. Which of them clears the token is
// left to the scheduler; the equality guard that keeps the others from
// clearing its replacement is pinned by the invalidate unit test.
func TestAnthropicWIFSingleFlightReExchange(t *testing.T) {
	t.Parallel()

	const callers = 8

	fake := newWIFServer(t)
	fake.rotateTokens = true

	// The first token is accepted until the cache is warm, then rejected.
	var armed atomic.Bool

	rejected := "Bearer " + wifRotatedToken(1)
	fake.messagesStatus = func(_ int64, authorization string) int {
		if armed.Load() && authorization == rejected {
			return http.StatusUnauthorized
		}

		return http.StatusOK
	}

	// Every first attempt is held until all of them have arrived, so all of
	// them carry the token that is about to be rejected.
	var arrived atomic.Int64

	release := make(chan struct{})
	fake.messagesGate = func(authorization string) {
		if !armed.Load() || authorization != rejected {
			return
		}

		if arrived.Add(1) == callers {
			close(release)
		}

		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}

	provider := newWIFProvider(fake, &wifIdentity{})

	wifWarm(t, provider)
	armed.Store(true)

	var (
		wg     sync.WaitGroup
		failed atomic.Int64
	)

	for range callers {
		wg.Go(func() {
			if _, err := provider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
				failed.Add(1)
			}
		})
	}

	wg.Wait()

	if arrived.Load() != callers {
		t.Fatalf("first attempts with the rejected token = %d, want %d", arrived.Load(), callers)
	}

	if failed.Load() != 0 {
		t.Errorf("%d concurrent completions failed", failed.Load())
	}

	if fake.exchanges.Load() != 2 || fake.completions.Load() != 1+2*callers {
		t.Errorf("exchanges = %d, completions = %d; want 2 and %d",
			fake.exchanges.Load(), fake.completions.Load(), 1+2*callers)
	}
}

// Only the token that failed is cleared; a newer one left by another caller
// stays.
func TestAnthropicWIFInvalidateClearsOnlyTheRejectedToken(t *testing.T) {
	t.Parallel()

	expiry := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	provider := &AnthropicWIF{token: "newer", expiry: expiry}

	provider.invalidate("older")

	if provider.token != "newer" || !provider.expiry.Equal(expiry) {
		t.Errorf("after invalidating another token: token = %q, expiry = %v; want both kept",
			provider.token, provider.expiry)
	}

	provider.invalidate("newer")

	if provider.token != "" || !provider.expiry.IsZero() {
		t.Errorf("after invalidating the cached token: token = %q, expiry = %v; want both cleared",
			provider.token, provider.expiry)
	}
}

// Neither token reaches a log line or an error, on the success path or on any
// failure path; the usage and exchange lines carry what they promise.
func TestAnthropicWIFNeverLogsAToken(t *testing.T) {
	t.Parallel()

	logger, logs := wifLogger()

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

	// A fresh token rejected; a cached token rejected and recovered; a cached
	// token rejected and its replacement rejected too.
	for _, tc := range []struct {
		warm     bool
		statuses []int
	}{
		{false, []int{http.StatusUnauthorized}},
		{true, []int{http.StatusOK, http.StatusUnauthorized}},
		{true, []int{http.StatusOK, http.StatusUnauthorized, http.StatusUnauthorized}},
	} {
		rejecting := newWIFServer(t)
		rejecting.rotateTokens = true
		rejecting.messagesStatus = wifStatuses(tc.statuses...)
		rejectingProvider := newWIFProvider(rejecting, &wifIdentity{})
		rejectingProvider.Logger = logger

		if tc.warm {
			wifWarm(t, rejectingProvider)
		}

		if _, err := rejectingProvider.Complete(t.Context(), Prompt{System: "s", User: "u"}); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) != 7 {
		t.Fatalf("errors = %v, want one per failure path", errs)
	}

	output := logs.String()

	for _, secret := range []string{wifTestAccessToken, wifTestIdentityToken, "oat01", "SECRET-PROMPT-TEXT"} {
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
		`"msg":"anthropic access token rejected; re-exchanging","status":401`,
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
