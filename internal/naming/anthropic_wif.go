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
	"sync"
	"time"
)

// AnthropicWIF calls the Messages API with no key at all.
//
// The runtime service account proves who it is with a Google identity token,
// and Anthropic's workload identity federation exchanges that token for a
// short-lived access token bound to an Anthropic service account. Nothing
// long-lived exists to leak: the identity token is minted by the metadata
// server, the access token expires within the hour, and LLM_API_KEY is not
// read.
//
// It is a provider of its own rather than a mode of [Anthropic]: the keyed
// provider sends a temperature, is pinned to a Haiku-class model and
// authenticates with x-api-key, and every one of those differs here. One
// request shape per provider is what an A/B of narrators rests on.
//
// The Go SDK is not used for the same reason it is not used elsewhere in this
// package: the exchange is one POST and the completion another, and neither is
// worth a dependency.
//
//	exchange:   POST https://api.anthropic.com/v1/oauth/token
//	            grant_type urn:ietf:params:oauth:grant-type:jwt-bearer
//	completion: POST https://api.anthropic.com/v1/messages
//	            Authorization: Bearer, anthropic-version: 2023-06-01
type AnthropicWIF struct {
	// Client is the HTTP client for both calls. Nil means a default one with
	// a timeout.
	Client *http.Client

	// IdentityToken mints a Google identity token for the audience. The
	// caller supplies it, which keeps this package free of a cloud SDK and of
	// the metadata server's address. Required.
	IdentityToken func(ctx context.Context, audience string) (string, error)

	// Audience is the identity token's audience, which the federation rule
	// requires exactly. Empty means [DefaultAnthropicWIFAudience].
	Audience string

	// FederationRuleID, OrganizationID and ServiceAccountID name the
	// federation rule, the organization and the Anthropic service account the
	// exchange is for. Identifiers, not secrets; all three are required.
	FederationRuleID string
	OrganizationID   string
	ServiceAccountID string

	// WorkspaceID scopes the access token to one workspace. Empty sends no
	// workspace_id.
	WorkspaceID string

	// Model is the model ID. Empty means [DefaultAnthropicWIFModel].
	Model string

	// BaseURL overrides the API host in tests. Empty means
	// https://api.anthropic.com.
	BaseURL string

	// Logger receives one usage line per completion and one line per
	// exchange. Nil logs nothing. Neither line carries a token.
	Logger *slog.Logger

	// now is the clock the cache reads. Nil means time.Now.
	now func() time.Time

	// mu guards the cached access token, and is held across an exchange so
	// that concurrent callers share one exchange rather than each starting
	// their own.
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// DefaultAnthropicWIFModel is the model this provider ships with.
//
// Verified against
// https://platform.claude.com/docs/en/about-claude/models/overview, read
// 2026-10-18.
const DefaultAnthropicWIFModel = "claude-fable-5-1"

// DefaultAnthropicWIFAudience is the identity token's audience the federation
// rule expects.
const DefaultAnthropicWIFAudience = "https://api.anthropic.com"

// anthropicWIFProvider names this provider in its errors.
const anthropicWIFProvider = "anthropic-wif"

// anthropicWIFEffort asks for as little deliberation as the model allows.
// Naming a ride needs no chain of reasoning, and thinking tokens are billed
// inside max_tokens.
const anthropicWIFEffort = "low"

// jwtBearerGrant is the RFC 7523 grant type the exchange accepts.
const jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer" //nolint:gosec // a grant type name, not a credential

// tokenRefreshMargin is how long before expiry a cached access token is
// replaced. A token that expires between the check and the call would fail
// the ride for nothing.
const tokenRefreshMargin = 120 * time.Second

// Anthropic's content and stop vocabulary for this provider.
const (
	contentTypeText   = "text"
	stopReasonRefusal = "refusal"
)

// Name identifies the provider in logs: the provider, the model, the effort,
// and — through the provider name itself — the keyless authentication.
func (a *AnthropicWIF) Name() string {
	return anthropicWIFProvider + "/" + a.model() + " effort=" + anthropicWIFEffort
}

func (a *AnthropicWIF) model() string {
	if a.Model == "" {
		return DefaultAnthropicWIFModel
	}

	return a.Model
}

func (a *AnthropicWIF) audience() string {
	if a.Audience == "" {
		return DefaultAnthropicWIFAudience
	}

	return a.Audience
}

// exchangeTimeout bounds one token exchange. Shorter than a completion's
// bound: the exchange carries no model work, only a signature check.
const exchangeTimeout = 10 * time.Second

func (a *AnthropicWIF) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}

	return &http.Client{Timeout: defaultTimeout}
}

func (a *AnthropicWIF) clock() time.Time {
	if a.now == nil {
		return time.Now()
	}

	return a.now()
}

func (a *AnthropicWIF) url(path string) string {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}

	return strings.TrimRight(base, "/") + path
}

type anthropicExchangeRequest struct {
	GrantType        string `json:"grant_type"`
	Assertion        string `json:"assertion"`
	FederationRuleID string `json:"federation_rule_id"`
	OrganizationID   string `json:"organization_id"`
	ServiceAccountID string `json:"service_account_id"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
}

type anthropicExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

// anthropicWIFRequest is the whole request. No temperature, no thinking
// field, no tools and no JSON mode: the effort is the one sampling control,
// and the validator is the output contract.
type anthropicWIFRequest struct {
	Model        string                `json:"model"`
	MaxTokens    int                   `json:"max_tokens"`
	System       string                `json:"system"`
	Messages     []anthropicMessage    `json:"messages"`
	OutputConfig anthropicOutputConfig `json:"output_config"`
}

type anthropicWIFResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category string `json:"category"`
	} `json:"stop_details"`
	Usage struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		OutputTokensDetails struct {
			ThinkingTokens int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// Complete sends the prompt to the Messages API and returns the model's text.
//
// A failed exchange is returned like any other provider error, so the caller
// leaves the activity queued. Nothing here retries: the next sweep is the
// retry.
func (a *AnthropicWIF) Complete(ctx context.Context, prompt Prompt) (string, error) {
	token, err := a.accessToken(ctx)
	if err != nil {
		return "", err
	}

	payload := anthropicWIFRequest{
		Model:        a.model(),
		MaxTokens:    maxOutputTokens,
		System:       prompt.System,
		Messages:     []anthropicMessage{{Role: roleUser, Content: prompt.User}},
		OutputConfig: anthropicOutputConfig{Effort: anthropicWIFEffort},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("naming: anthropic-wif: encode request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url("/v1/messages"), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("naming: anthropic-wif: build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Anthropic-Version", anthropicVersion)

	start := time.Now()

	response, err := a.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("naming: anthropic-wif: request: %w", err)
	}
	defer drainAndClose(response)

	if response.StatusCode != http.StatusOK {
		// The body can echo the request, which carries the prompt, so it is
		// never included.
		return "", fmt.Errorf("naming: anthropic-wif: %w", statusError(response))
	}

	var decoded anthropicWIFResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAnthropicResponseBytes)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("naming: anthropic-wif: decode response: %w", err)
	}

	// Logged before any verdict on the response, because the calls where the
	// ceiling mattered — a truncation, a refusal — are the ones whose token
	// split answers why.
	if a.Logger != nil {
		a.Logger.Info("anthropic usage",
			"model", a.model(),
			"input_tokens", decoded.Usage.InputTokens,
			"output_tokens", decoded.Usage.OutputTokens,
			"thinking_tokens", decoded.Usage.OutputTokensDetails.ThinkingTokens,
			"stop_reason", decoded.StopReason,
			"latency_ms", time.Since(start).Milliseconds())
	}

	// The stop reason before the text: a truncated response holds half an
	// object or nothing, and "not JSON" or "no title" would name the wrong
	// cause.
	switch decoded.StopReason {
	case stopReasonMaxTokens:
		return "", truncatedError(anthropicWIFProvider)
	case stopReasonRefusal:
		category := ""
		if decoded.StopDetails != nil {
			category = decoded.StopDetails.Category
		}

		return "", fmt.Errorf("naming: anthropic-wif: refused (%s)", category)
	}

	var text strings.Builder

	// Thinking blocks are the model's deliberation, not its answer.
	for _, block := range decoded.Content {
		if block.Type == contentTypeText {
			text.WriteString(block.Text)
		}
	}

	if text.Len() == 0 {
		return "", ErrNoTitle
	}

	return text.String(), nil
}

// accessToken returns a cached access token, or exchanges for a new one when
// fewer than [tokenRefreshMargin] remain.
//
// The lock is held across the exchange: callers that arrive while one is in
// flight wait for it and share its token. A failed exchange caches nothing,
// so the next caller exchanges again.
func (a *AnthropicWIF) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.expiry.Sub(a.clock()) >= tokenRefreshMargin {
		return a.token, nil
	}

	token, expiry, err := a.exchange(ctx)
	if err != nil {
		return "", err
	}

	a.token, a.expiry = token, expiry

	return token, nil
}

// exchange trades a fresh identity token for an access token.
//
// The identity token is minted per exchange and never reused. A Google token
// carries no jti, but presenting one twice would be a replay the exchange is
// entitled to refuse.
//
// No error names either token: the assertion is a bearer credential for as
// long as it lives, and the access token is one for the hour after.
func (a *AnthropicWIF) exchange(ctx context.Context) (string, time.Time, error) {
	if a.IdentityToken == nil {
		return "", time.Time{}, errors.New("naming: anthropic-wif: no identity token source configured")
	}

	if a.FederationRuleID == "" || a.OrganizationID == "" || a.ServiceAccountID == "" {
		return "", time.Time{}, errors.New(
			"naming: anthropic-wif: federation rule, organization and service account are required")
	}

	assertion, err := a.IdentityToken(ctx, a.audience())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: identity token: %w", err)
	}

	if assertion == "" {
		return "", time.Time{}, errors.New("naming: anthropic-wif: identity token: empty")
	}

	body, err := json.Marshal(anthropicExchangeRequest{
		GrantType:        jwtBearerGrant,
		Assertion:        assertion,
		FederationRuleID: a.FederationRuleID,
		OrganizationID:   a.OrganizationID,
		ServiceAccountID: a.ServiceAccountID,
		WorkspaceID:      a.WorkspaceID,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: encode exchange: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url("/v1/oauth/token"), bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: build exchange: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")

	// The exchange has its own, shorter bound than a completion: a hung token
	// endpoint must not spend the sweep's budget twice over per ride.
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()

	request = request.WithContext(ctx)

	// Issued is taken before the request leaves, so the expiry computed from
	// expires_in is never later than the one the server meant: the server
	// counts from when it minted the token, which is before any byte of the
	// response arrived here.
	issued := a.clock()
	start := time.Now()

	response, err := a.client().Do(request)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: exchange: %w", err)
	}
	defer drainAndClose(response)

	// A denial is an opaque 401 whatever its cause; the status is all there
	// is to report, and the body is never quoted.
	if response.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: exchange: %w", statusError(response))
	}

	var decoded anthropicExchangeResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAnthropicResponseBytes)).Decode(&decoded); err != nil {
		// A decode error names a character or a type, never a value.
		return "", time.Time{}, fmt.Errorf("naming: anthropic-wif: decode exchange: %w", err)
	}

	if decoded.AccessToken == "" || decoded.ExpiresIn <= 0 || !strings.EqualFold(decoded.TokenType, "Bearer") {
		return "", time.Time{}, fmt.Errorf(
			"naming: anthropic-wif: malformed exchange response (status %d)", response.StatusCode)
	}

	if a.Logger != nil {
		a.Logger.Info("anthropic exchange",
			"expires_in", decoded.ExpiresIn,
			"scope", capScope(decoded.Scope),
			"latency_ms", time.Since(start).Milliseconds())
	}

	return decoded.AccessToken, issued.Add(time.Duration(decoded.ExpiresIn) * time.Second), nil
}

// capScope bounds the server-returned scope before it reaches a log. A scope
// is a few words; this is for a response that is not one.
func capScope(scope string) string {
	const limit = 128

	if runes := []rune(scope); len(runes) > limit {
		return string(runes[:limit])
	}

	return scope
}
