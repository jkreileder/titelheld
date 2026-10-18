package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fetch asks the metadata server for a full-format identity token for the
// audience, with the header that proves it is not a forwarded request, and
// returns the body trimmed.
func TestMetadataIdentityToken(t *testing.T) {
	t.Parallel()

	var gotPath, gotAudience, gotFormat, gotFlavor, gotMethod string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotFlavor = r.Method, r.URL.Path, r.Header.Get("Metadata-Flavor")
		gotAudience, gotFormat = r.URL.Query().Get("audience"), r.URL.Query().Get("format")

		_, _ = io.WriteString(w, "eyJ-fake-identity-token\n")
	}))
	defer server.Close()

	token, err := metadataIdentity{BaseURL: server.URL, Client: server.Client()}.Token(t.Context(), "https://api.anthropic.com")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}

	if token != "eyJ-fake-identity-token" {
		t.Errorf("token = %q, want the body trimmed", token)
	}

	if gotMethod != http.MethodGet || gotPath != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
		gotAudience != "https://api.anthropic.com" || gotFormat != "full" || gotFlavor != "Google" {
		t.Errorf("request = %s %s audience=%q format=%q flavor=%q", gotMethod, gotPath, gotAudience, gotFormat, gotFlavor)
	}
}

// A non-200 names the status and never the body; an empty body is an error.
func TestMetadataIdentityTokenFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusNotFound, "SECRET-BODY", "404"},
		{http.StatusOK, " \n", "empty"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))

		_, err := metadataIdentity{BaseURL: server.URL, Client: server.Client()}.Token(t.Context(), "aud")

		server.Close()

		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("status %d: err = %v, want %q without the body", tc.status, err, tc.want)
		}
	}
}

// The real fetch has a bounded default client and the metadata server's
// address.
func TestMetadataIdentityDefaults(t *testing.T) {
	t.Parallel()

	if timeout := (metadataIdentity{}).client().Timeout; timeout != 10*time.Second {
		t.Errorf("default client timeout = %v, want 10s", timeout)
	}

	if metadataBaseURL != "http://metadata.google.internal" {
		t.Errorf("base URL = %q", metadataBaseURL)
	}
}
