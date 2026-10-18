package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// metadataBaseURL is the Cloud Run metadata server. Plain http is its only
// scheme: the address resolves only from inside the instance.
const metadataBaseURL = "http://metadata.google.internal"

// metadataTimeout bounds an identity-token fetch. The metadata server answers
// in milliseconds; a fetch that takes longer is not going to succeed.
const metadataTimeout = 10 * time.Second

// maxIdentityTokenBytes caps what a fetch reads. A Google identity token is
// about a kilobyte.
const maxIdentityTokenBytes = 1 << 16

// metadataIdentity mints Google identity tokens for the runtime service
// account from the metadata server, by hand with net/http, so the naming
// layer's keyless Anthropic provider needs no cloud SDK and no new module.
type metadataIdentity struct {
	// BaseURL overrides the metadata server in tests. Empty means the real
	// one.
	BaseURL string

	// Client overrides the HTTP client in tests. Nil means one with
	// [metadataTimeout].
	Client *http.Client
}

func (m metadataIdentity) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}

	return &http.Client{Timeout: metadataTimeout}
}

// Token fetches a fresh identity token for the audience. format=full asks
// for the claims the federation rule matches on, the email among them.
//
// Neither the token nor the response body appears in an error.
func (m metadataIdentity) Token(ctx context.Context, audience string) (string, error) {
	base := m.BaseURL
	if base == "" {
		base = metadataBaseURL
	}

	query := url.Values{"audience": {audience}, "format": {"full"}}
	endpoint := strings.TrimRight(base, "/") +
		"/computeMetadata/v1/instance/service-accounts/default/identity?" + query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("metadata identity: build request: %w", err)
	}

	request.Header.Set("Metadata-Flavor", "Google")

	response, err := m.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("metadata identity: request: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxIdentityTokenBytes))
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata identity: unexpected status %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxIdentityTokenBytes))
	if err != nil {
		return "", fmt.Errorf("metadata identity: read response: %w", err)
	}

	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", errors.New("metadata identity: empty token")
	}

	return token, nil
}
