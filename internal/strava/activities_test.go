package strava

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A page comes back decoded, with the athlete lifted out of the nested object.
func TestListActivitiesReturnsAPage(t *testing.T) {
	t.Parallel()

	var query string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id": 90000000001, "name": "Gegenwind bis Musterdorf",
			 "sport_type": "GravelRide", "start_date": "2026-08-15T12:30:00Z",
			 "athlete": {"id": 42424242}},
			{"id": 90000000002, "name": "Morning Ride",
			 "sport_type": "Ride", "start_date": "2026-08-14T06:00:00Z",
			 "athlete": {"id": 42424242}}
		]`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	activities, err := client.ListActivities(t.Context(), 2, 50)
	if err != nil {
		t.Fatalf("ListActivities: %v", err)
	}

	if len(activities) != 2 {
		t.Fatalf("%d activities, want 2", len(activities))
	}

	if activities[0].Name != "Gegenwind bis Musterdorf" {
		t.Errorf("name = %q", activities[0].Name)
	}

	if activities[0].AthleteID != 42424242 {
		t.Errorf("AthleteID = %d, want it lifted from the nested athlete", activities[0].AthleteID)
	}

	if !strings.Contains(query, "page=2") || !strings.Contains(query, "per_page=50") {
		t.Errorf("query = %q, want the page and size asked for", query)
	}
}

// An empty page is the end of the history, not an error.
func TestListActivitiesEndsOnAnEmptyPage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	activities, err := client.ListActivities(t.Context(), 9, 200)
	if err != nil {
		t.Fatalf("ListActivities: %v", err)
	}

	if len(activities) != 0 {
		t.Errorf("%d activities, want none", len(activities))
	}
}

// Paging arguments are checked here rather than by Strava.
func TestListActivitiesRejectsBadPaging(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("Strava was called with arguments that should have been refused")
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	for _, tc := range []struct{ page, perPage int }{
		{page: 0, perPage: 100},
		{page: -1, perPage: 100},
		{page: 1, perPage: 0},
		{page: 1, perPage: -5},
		{page: 1, perPage: MaxActivitiesPerPage + 1},
	} {
		t.Run(fmt.Sprintf("page=%d/per_page=%d", tc.page, tc.perPage), func(t *testing.T) {
			t.Parallel()

			if _, err := client.ListActivities(t.Context(), tc.page, tc.perPage); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// A listing is a read, so dry run must not block it.
func TestListActivitiesWorksInDryRun(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"Eine Runde"}]`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.ListActivities(t.Context(), 1, 10); err != nil {
		t.Fatalf("ListActivities in dry run: %v", err)
	}
}

// One JSON value and nothing after it, and a body that ends told from one
// that hit the cap — the same contract as the gear read.
func TestListActivitiesRejectsTrailingData(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "a second array", body: `[{"id":1}][{"id":2}]`},
		{name: "trailing junk", body: `[{"id":1}] not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := newTestClient(t, server, WriteModeDryRun)

			if _, err := client.ListActivities(t.Context(), 1, 10); err == nil {
				t.Errorf("%s was accepted", tc.name)
			}
		})
	}
}

// A malformed body is reported rather than read as an empty history, which
// would look exactly like the end of the listing and stop the import early.
func TestListActivitiesRejectsAMalformedBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":`))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.ListActivities(t.Context(), 1, 10); err == nil {
		t.Error("a malformed page was accepted")
	}
}

// A transport failure is reported rather than read as the end of the history.
//
// An empty page ends the listing, so an error that came back as "no
// activities" would stop an import early and look like success.
func TestListActivitiesReportsATransportFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.ListActivities(t.Context(), 1, 10); err == nil {
		t.Error("a 500 was read as the end of the history")
	}
}

// A page padded past the byte cap is refused, not truncated.
//
// io.LimitReader reports EOF once its budget is gone, so without the headroom
// check a body larger than the cap would decode whatever fitted and look
// complete.
func TestListActivitiesRejectsAnOversizedPage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"Eine Runde"}]`))
		_, _ = w.Write([]byte(strings.Repeat(" ", maxActivityListBytes)))
	}))
	defer server.Close()

	client := newTestClient(t, server, WriteModeDryRun)

	if _, err := client.ListActivities(t.Context(), 1, 10); err == nil {
		t.Error("a page past the byte cap was accepted")
	}
}
