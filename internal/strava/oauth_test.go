package strava

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fixedNow is a stable clock for expiry assertions.
var fixedNow = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func testOAuth(server *httptest.Server) *OAuth {
	oauth := &OAuth{
		ClientID:     "12345",
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://example.invalid/auth/callback",
		Now:          func() time.Time { return fixedNow },
	}

	if server != nil {
		oauth.BaseURL = server.URL
		oauth.HTTPClient = server.Client()
	}

	return oauth
}

func TestAuthorizeURL(t *testing.T) {
	t.Parallel()

	raw := testOAuth(nil).AuthorizeURL("state-abc")

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := parsed.Host; got != "www.strava.com" {
		t.Errorf("host = %q", got)
	}
	if got := parsed.Path; got != "/oauth/authorize" {
		t.Errorf("path = %q", got)
	}

	query := parsed.Query()
	want := map[string]string{
		"client_id":       "12345",
		"redirect_uri":    "https://example.invalid/auth/callback",
		"response_type":   "code",
		"scope":           "activity:read_all,activity:write",
		"approval_prompt": "force",
		"state":           "state-abc",
	}

	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}

	if strings.Contains(raw, "test-client-secret") {
		t.Error("the authorize URL must never carry the client secret")
	}
}

func TestExchange(t *testing.T) {
	t.Parallel()

	var gotForm atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}

		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		gotForm.Store(r.PostForm.Encode())

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"token_type": "Bearer",
			"expires_at": 1787072400,
			"expires_in": 21600,
			"refresh_token": "refresh-one",
			"access_token": "access-one",
			"scope": "read,activity:read_all,activity:write",
			"athlete": {"id": 4242}
		}`))
	}))
	defer server.Close()

	token, err := testOAuth(server).Exchange(t.Context(), "auth-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	form, _ := url.ParseQuery(gotForm.Load().(string))
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "auth-code" {
		t.Errorf("form = %v", form)
	}
	if form.Get("client_secret") != "test-client-secret" {
		t.Error("client secret must be sent in the POST body")
	}

	if token.AccessToken != "access-one" || token.RefreshToken != "refresh-one" {
		t.Errorf("token = %+v", token)
	}
	if token.AthleteID != 4242 {
		t.Errorf("AthleteID = %d", token.AthleteID)
	}
	if want := time.Unix(1787072400, 0).UTC(); !token.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", token.ExpiresAt, want)
	}
	if !token.HasScope(ScopeActivityWrite) || !token.HasScope(ScopeActivityReadAll) {
		t.Errorf("scopes = %v", token.Scopes)
	}
	if got := token.MissingScopes(); len(got) != 0 {
		t.Errorf("MissingScopes() = %v, want none", got)
	}
}

func TestRefreshRotatesTheRefreshToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}

		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.PostForm.Get("refresh_token"); got != "refresh-one" {
			t.Errorf("refresh_token = %q", got)
		}

		_, _ = w.Write([]byte(`{
			"expires_in": 21600,
			"refresh_token": "refresh-two",
			"access_token": "access-two"
		}`))
	}))
	defer server.Close()

	token, err := testOAuth(server).Refresh(t.Context(), "refresh-one")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if token.RefreshToken != "refresh-two" {
		t.Errorf("RefreshToken = %q, want the rotated value", token.RefreshToken)
	}
	// No expires_at in this response, so expires_in drives the expiry.
	if want := fixedNow.Add(21600 * time.Second); !token.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", token.ExpiresAt, want)
	}
}

func TestTokenRequestErrorsHideTheBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Strava echoes submitted fields in some error payloads.
		_, _ = w.Write([]byte(`{"errors":[{"field":"client_secret: test-client-secret"}]}`))
	}))
	defer server.Close()

	_, err := testOAuth(server).Exchange(t.Context(), "bad-code")
	if err == nil {
		t.Fatal("Exchange = nil error, want error")
	}

	if strings.Contains(err.Error(), "test-client-secret") {
		t.Errorf("error leaked the response body: %v", err)
	}
}

func TestTokenResponseMustCarryBothTokens(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"only-access"}`))
	}))
	defer server.Close()

	if _, err := testOAuth(server).Exchange(t.Context(), "code"); !errors.Is(err, ErrIncompleteToken) {
		t.Fatalf("error = %v, want ErrIncompleteToken", err)
	}
}

func TestTokenResponseBadJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{`))
	}))
	defer server.Close()

	if _, err := testOAuth(server).Exchange(t.Context(), "code"); err == nil {
		t.Fatal("Exchange with truncated JSON = nil error, want error")
	}
}

func TestTokenRequestTransportError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	oauth := testOAuth(server)
	server.Close()

	if _, err := oauth.Exchange(t.Context(), "code"); err == nil {
		t.Fatal("Exchange against a closed server = nil error, want error")
	}
}

func TestTokenResponseWithoutExpiry(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r"}`))
	}))
	defer server.Close()

	token, err := testOAuth(server).Exchange(t.Context(), "code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if !token.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt = %v, want the zero time", token.ExpiresAt)
	}
	// A token with no known expiry must be treated as needing a refresh.
	if !token.Expired(fixedNow, 0) {
		t.Error("a token with no expiry must report as expired")
	}
}

func TestOAuthDefaults(t *testing.T) {
	t.Parallel()

	oauth := &OAuth{}

	if got := oauth.baseURL(); got != DefaultOAuthBaseURL {
		t.Errorf("baseURL() = %q", got)
	}
	if oauth.httpClient() == nil {
		t.Error("httpClient() = nil")
	}
	if oauth.now().IsZero() {
		t.Error("now() returned the zero time")
	}

	trailing := &OAuth{BaseURL: "https://example.invalid/"}
	if got := trailing.baseURL(); got != "https://example.invalid" {
		t.Errorf("baseURL() = %q, want the trailing slash trimmed", got)
	}
}

func TestParseScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want []string
	}{
		{in: "read,activity:read_all,activity:write",
			want: []string{"read", "activity:read_all", "activity:write"}},
		{in: "read activity:write", want: []string{"read", "activity:write"}},
		{in: " read , , activity:write ", want: []string{"read", "activity:write"}},
		{in: "", want: []string{}},
	}

	for _, tt := range tests {
		if got := ParseScopes(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("ParseScopes(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestTokenExpiredAndScopes(t *testing.T) {
	t.Parallel()

	token := Token{
		ExpiresAt: fixedNow.Add(time.Hour),
		Scopes:    []string{ScopeActivityReadAll},
	}

	if token.Expired(fixedNow, time.Minute) {
		t.Error("a token an hour from expiry must not report as expired")
	}
	if !token.Expired(fixedNow, 2*time.Hour) {
		t.Error("leeway past the expiry must report as expired")
	}
	if !token.Expired(token.ExpiresAt, 0) {
		t.Error("a token at exactly its expiry must report as expired")
	}

	missing := token.MissingScopes()
	if !slices.Equal(missing, []string{ScopeActivityWrite}) {
		t.Errorf("MissingScopes() = %v, want [activity:write]", missing)
	}
}

func TestTokenRequestWithUnbuildableURL(t *testing.T) {
	t.Parallel()

	oauth := &OAuth{ClientID: "1", ClientSecret: "s", BaseURL: "://not-a-url"}

	if _, err := oauth.Exchange(t.Context(), "code"); err == nil {
		t.Fatal("Exchange with an unparseable base URL = nil error, want error")
	}
}

// The same ceiling applies to a token response, which is structurally tiny.
func TestTokenResponseStopsAtTheSizeLimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`)

		chunk := strings.Repeat("a", 4096)
		for written := 0; written < maxTokenBytes*2; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}

		_, _ = io.WriteString(w, `"}`)
	}))
	defer server.Close()

	oauth := &OAuth{ClientID: "1", ClientSecret: "s", BaseURL: server.URL, HTTPClient: server.Client()}

	if _, err := oauth.Exchange(t.Context(), "code"); err == nil {
		t.Error("Exchange on an oversized body = nil error, want a decode failure")
	}
}
