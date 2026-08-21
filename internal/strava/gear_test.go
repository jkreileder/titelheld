package strava

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The gear name comes back, and it is the only field read.
func TestGetGearReturnsTheName(t *testing.T) {
	t.Parallel()

	var requested string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "b1234567",
			"primary": true,
			"name": "Pink Panther",
			"nickname": "the gravel bike",
			"resource_state": 3,
			"distance": 12345678,
			"brand_name": "Musterrad",
			"model_name": "Mustermodell",
			"frame_type": 2,
			"description": "a description that is none of our business"
		}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	gear, err := client.GetGear(t.Context(), "b1234567")
	if err != nil {
		t.Fatalf("GetGear: %v", err)
	}

	if gear.Name != "Pink Panther" {
		t.Errorf("Name = %q, want %q", gear.Name, "Pink Panther")
	}

	if requested != "/gear/b1234567" {
		t.Errorf("requested %q, want /gear/b1234567", requested)
	}
}

// An activity recorded without gear is not an error.
func TestGetGearWithoutAnID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("Strava was called for an activity with no gear")
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	for _, id := range []string{"", "   "} {
		gear, err := client.GetGear(t.Context(), id)
		if err != nil {
			t.Fatalf("GetGear(%q): %v", id, err)
		}

		if gear != (Gear{}) {
			t.Errorf("GetGear(%q) = %+v, want the zero value", id, gear)
		}
	}
}

// The ID comes from an activity, so it cannot be trusted to stay in its path
// segment. A gear ID of "../athlete" must not address /athlete.
func TestGetGearEscapesTheID(t *testing.T) {
	t.Parallel()

	var requested string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.EscapedPath()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","name":"n"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetGear(t.Context(), "../athlete"); err != nil {
		t.Fatalf("GetGear: %v", err)
	}

	segment, ok := strings.CutPrefix(requested, "/gear/")
	if !ok {
		t.Fatalf("the request left the gear path: %q", requested)
	}

	// What matters is that the ID stays one segment. ".." is harmless once the
	// separator is escaped; an unescaped "/" is what would address /athlete.
	if strings.Contains(segment, "/") {
		t.Errorf("the gear ID escaped its path segment: %q", requested)
	}
}

// A gear lookup is a read, so dry run must not block it.
func TestGetGearWorksInDryRun(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"b1","name":"Musterrad"}`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	gear, err := client.GetGear(t.Context(), "b1")
	if err != nil {
		t.Fatalf("GetGear in dry run: %v", err)
	}

	if gear.Name != "Musterrad" {
		t.Errorf("Name = %q, want %q", gear.Name, "Musterrad")
	}
}

// A malformed body is reported rather than read as an empty name.
func TestGetGearRejectsAMalformedBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.GetGear(t.Context(), "b1"); err == nil {
		t.Error("a malformed gear response was accepted")
	}
}

// One JSON value, and nothing after it.
//
// Decode stops at the end of the first value, so a valid object followed by
// anything at all would otherwise be accepted silently — and this reads a
// response the service did not produce.
func TestGetGearRejectsTrailingData(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "a second object", body: `{"id":"b1","name":"Musterrad"}{"id":"b2","name":"Anderes"}`},
		{name: "trailing junk", body: `{"id":"b1","name":"Musterrad"} not json`},
		{name: "an array after it", body: `{"id":"b1","name":"Musterrad"}[1,2,3]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := newTestClient(t, server, WriteModeDryRun)

			if _, err := client.GetGear(t.Context(), "b1"); err == nil {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
}

// Whitespace after the object is not trailing data.
func TestGetGearAcceptsTrailingWhitespace(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"id\":\"b1\",\"name\":\"Musterrad\"}\n\n  "))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	gear, err := client.GetGear(t.Context(), "b1")
	if err != nil {
		t.Fatalf("GetGear: %v", err)
	}

	if gear.Name != "Musterrad" {
		t.Errorf("Name = %q, want %q", gear.Name, "Musterrad")
	}
}
