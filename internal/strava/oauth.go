package strava

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxTokenBytes caps what a decode will read from a token response.
const maxTokenBytes = 1 << 16

// OAuth request parameter names.
const (
	paramClientID     = "client_id"
	paramClientSecret = "client_secret"
	paramRefreshToken = "refresh_token"
	paramGrantType    = "grant_type"
	grantAuthCode     = "authorization_code"
	grantRefreshToken = "refresh_token"
	responseTypeCode  = "code"
)

// DefaultOAuthBaseURL is Strava's OAuth host. Overridable so tests can point at
// an httptest server.
const DefaultOAuthBaseURL = "https://www.strava.com"

// OAuth performs Strava's authorization-code flow.
//
// Only the one-time bootstrap uses AuthorizeURL and Exchange; Refresh runs for
// the lifetime of the service.
type OAuth struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string

	// BaseURL defaults to [DefaultOAuthBaseURL].
	BaseURL string

	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client

	// Now defaults to time.Now. Injected so token expiry is testable.
	Now func() time.Time
}

func (o *OAuth) baseURL() string {
	if o.BaseURL == "" {
		return DefaultOAuthBaseURL
	}

	return strings.TrimSuffix(o.BaseURL, "/")
}

func (o *OAuth) httpClient() *http.Client {
	if o.HTTPClient == nil {
		return &http.Client{Timeout: 30 * time.Second}
	}

	return o.HTTPClient
}

func (o *OAuth) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}

	return o.Now()
}

// AuthorizeURL is the URL the athlete visits once to grant access.
//
// approval_prompt=force is deliberate: with "auto" Strava silently reuses an
// earlier grant, so re-running the bootstrap to add a missing scope would
// appear to succeed while granting nothing new.
func (o *OAuth) AuthorizeURL(state string) string {
	query := url.Values{
		paramClientID:     {o.ClientID},
		"redirect_uri":    {o.RedirectURL},
		"response_type":   {responseTypeCode},
		"scope":           {Scopes},
		"approval_prompt": {"force"},
		"state":           {state},
	}

	return o.baseURL() + "/oauth/authorize?" + query.Encode()
}

// Exchange trades the one-time authorization code for a token pair.
func (o *OAuth) Exchange(ctx context.Context, code string) (Token, error) {
	return o.token(ctx, url.Values{
		paramClientID:     {o.ClientID},
		paramClientSecret: {o.ClientSecret},
		responseTypeCode:  {code},
		paramGrantType:    {grantAuthCode},
	})
}

// Refresh exchanges a refresh token for a fresh pair.
//
// The response always carries a *new* refresh token and the one passed in stops
// working immediately, so the caller must persist the result before relying on
// it.
func (o *OAuth) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	return o.token(ctx, url.Values{
		paramClientID:     {o.ClientID},
		paramClientSecret: {o.ClientSecret},
		paramRefreshToken: {refreshToken},
		paramGrantType:    {grantRefreshToken},
	})
}

// tokenResponse mirrors Strava's /oauth/token payload.
type tokenResponse struct {
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	Scope        string `json:"scope"`
	Athlete      struct {
		ID int64 `json:"id"`
	} `json:"athlete"`
}

func (o *OAuth) token(ctx context.Context, form url.Values) (Token, error) {
	endpoint := o.baseURL() + "/oauth/token"

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("strava: build token request: %w", err)
	}

	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := o.httpClient().Do(request)
	if err != nil {
		return Token{}, fmt.Errorf("strava: token request: %w", err)
	}
	defer drainAndClose(response)

	if response.StatusCode != http.StatusOK {
		// The body can echo the submitted form, which includes the client
		// secret, so it is never included in the error.
		return Token{}, fmt.Errorf("strava: token request: %w", statusError(response.StatusCode))
	}

	var payload tokenResponse
	// A token response is two opaque strings, an integer and a short scope
	// list, so this ceiling is far above anything legitimate. It is here
	// because the decode runs before drainAndClose can bound the body.
	if err := json.NewDecoder(io.LimitReader(response.Body, maxTokenBytes)).Decode(&payload); err != nil {
		return Token{}, fmt.Errorf("strava: decode token response: %w", err)
	}

	if payload.AccessToken == "" || payload.RefreshToken == "" {
		return Token{}, ErrIncompleteToken
	}

	return Token{
		AthleteID:    payload.Athlete.ID,
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresAt:    o.expiry(payload),
		Scopes:       ParseScopes(payload.Scope),
	}, nil
}

// expiry prefers the absolute expires_at and falls back to expires_in.
func (o *OAuth) expiry(payload tokenResponse) time.Time {
	if payload.ExpiresAt > 0 {
		return time.Unix(payload.ExpiresAt, 0).UTC()
	}

	if payload.ExpiresIn > 0 {
		return o.now().Add(time.Duration(payload.ExpiresIn) * time.Second).UTC()
	}

	return time.Time{}
}

// ParseScopes splits a granted-scope list.
//
// Strava's redirect delivers scopes comma-separated while the token response
// documents them space-delimited, so both separators are accepted.
func ParseScopes(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' '
	})

	scopes := make([]string, 0, len(fields))

	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			scopes = append(scopes, trimmed)
		}
	}

	return scopes
}
