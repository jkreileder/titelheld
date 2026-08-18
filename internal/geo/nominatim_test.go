package geo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// All coordinates here are synthetic and all place names are invented.
var testPoint = Point{Lat: 0.0503, Lon: 0.0002}

func newNominatim(t *testing.T, server *httptest.Server) *Nominatim {
	t.Helper()

	client, err := NewNominatim(NominatimConfig{
		UserAgent:  "titelheld-test/1.0 (test@example.invalid)",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewNominatim: %v", err)
	}

	return client
}

// jsonServer answers every reverse request with the same body.
func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return server
}

// TestPointsOfInterestNeverReachAPlace is the privacy rule with teeth: a title
// must never be able to reveal where the athlete actually went.
func TestPointsOfInterestNeverReachAPlace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		forbidden []string
		wantName  string
	}{
		{
			name: "a medical practice is dropped",
			body: `{"category":"amenity","type":"doctors","name":"Praxis Dr. Musterfrau",
				"display_name":"Praxis Dr. Musterfrau, Musterstraße 7, Musterdorf",
				"address":{"amenity":"Praxis Dr. Musterfrau","house_number":"7",
				"road":"Musterstraße","village":"Musterdorf","state":"Musterregion",
				"country":"Testland"}}`,
			forbidden: []string{"Praxis", "Musterfrau", "Musterstraße", "7"},
			wantName:  "Musterdorf",
		},
		{
			name: "a shop is dropped",
			body: `{"category":"shop","type":"supermarket","name":"Edeka Mustermann",
				"address":{"shop":"Edeka Mustermann","road":"Beispielweg",
				"town":"Musterstadt","country":"Testland"}}`,
			forbidden: []string{"Edeka", "Mustermann", "Beispielweg"},
			wantName:  "Musterstadt",
		},
		{
			name: "an office is dropped",
			body: `{"category":"office","type":"company","name":"Musterfirma GmbH",
				"address":{"office":"Musterfirma GmbH","house_number":"12",
				"road":"Industriestraße","city":"Musterstadt","country":"Testland"}}`,
			forbidden: []string{"Musterfirma", "Industriestraße", "12"},
			wantName:  "Musterstadt",
		},
		{
			name: "a place of worship is dropped",
			body: `{"category":"amenity","type":"place_of_worship","name":"St. Muster",
				"address":{"amenity":"St. Muster","suburb":"Musterviertel","country":"Testland"}}`,
			forbidden: []string{"St. Muster"},
			wantName:  "Musterviertel",
		},
		{
			name: "a building and house number are dropped",
			body: `{"category":"building","type":"house","name":"Haus Muster",
				"address":{"building":"Haus Muster","house_number":"3","road":"Am Musterweg",
				"hamlet":"Musterweiler","country":"Testland"}}`,
			forbidden: []string{"Haus Muster", "Am Musterweg", "3"},
			wantName:  "Musterweiler",
		},
		{
			name: "the free-text display_name is never read",
			body: `{"category":"amenity","type":"clinic","name":"Klinik Muster",
				"display_name":"Klinik Muster, 9, Geheimstraße, Musterdorf, Testland",
				"address":{"amenity":"Klinik Muster","village":"Musterdorf","country":"Testland"}}`,
			forbidden: []string{"Klinik", "Geheimstraße", "9"},
			wantName:  "Musterdorf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newNominatim(t, jsonServer(t, tt.body))

			place, err := client.Reverse(t.Context(), testPoint)
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}

			if place.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", place.Name, tt.wantName)
			}

			// Every field of the result, concatenated, must contain none of it.
			all := strings.Join([]string{place.Name, place.Kind, place.Region, place.Country}, " ")
			for _, forbidden := range tt.forbidden {
				if strings.Contains(all, forbidden) {
					t.Errorf("the place %+v leaked %q", place, forbidden)
				}
			}
		})
	}
}

// Named natural features are the exception the spec allows: a river reveals
// nothing about the athlete.
func TestNaturalFeaturesAreKept(t *testing.T) {
	t.Parallel()

	client := newNominatim(t, jsonServer(t, `{"category":"waterway","type":"river",
		"name":"Musterfluss","address":{"county":"Musterkreis","state":"Musterregion",
		"country":"Testland"}}`))

	place, err := client.Reverse(t.Context(), testPoint)
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	// The county is more specific than the river in the address hierarchy, so
	// it wins the name; the river would be used only if nothing else resolved.
	if place.Name != "Musterkreis" {
		t.Errorf("Name = %q, want Musterkreis", place.Name)
	}
	if place.Region != "Musterregion" || place.Country != "Testland" {
		t.Errorf("place = %+v", place)
	}
}

func TestNaturalFeatureUsedWhenNoSettlementResolves(t *testing.T) {
	t.Parallel()

	client := newNominatim(t, jsonServer(t, `{"category":"waterway","type":"river",
		"name":"Musterfluss","address":{"country":"Testland"}}`))

	place, err := client.Reverse(t.Context(), testPoint)
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	// Kind is the specific feature type from the allow-list, never the coarse
	// category and never text the server chose.
	if place.Name != "Musterfluss" || place.Kind != "river" {
		t.Errorf("place = %+v, want the river", place)
	}
}

// An amenity name must not be promoted even when no settlement resolves.
func TestAmenityIsNotPromotedWhenNothingElseResolves(t *testing.T) {
	t.Parallel()

	client := newNominatim(t, jsonServer(t, `{"category":"amenity","type":"hospital",
		"name":"Musterklinik","address":{"country":"Testland"}}`))

	place, err := client.Reverse(t.Context(), testPoint)
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	if place.Name != "" {
		t.Errorf("Name = %q, want empty — an amenity is never a title", place.Name)
	}
	if place.Country != "Testland" {
		t.Errorf("Country = %q", place.Country)
	}
}

// TestRateLimitIsEnforced makes the 1 req/s policy a property of the code
// rather than a comment.
func TestRateLimitIsEnforced(t *testing.T) {
	t.Parallel()

	var (
		clock  = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
		slept  []time.Duration
		server = jsonServer(t, `{"address":{"village":"Musterdorf","country":"Testland"}}`)
	)

	client, err := NewNominatim(NominatimConfig{
		UserAgent:  "titelheld-test/1.0",
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			// A sleep advances the clock, as a real one would.
			clock = clock.Add(d)

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewNominatim: %v", err)
	}

	const requests = 5

	for range requests {
		if _, err := client.Reverse(t.Context(), testPoint); err != nil {
			t.Fatalf("Reverse: %v", err)
		}
	}

	// The first request goes out immediately; every one after it waits.
	if len(slept) != requests-1 {
		t.Fatalf("slept %d times for %d requests, want %d", len(slept), requests, requests-1)
	}

	for i, d := range slept {
		if d < MinRequestInterval {
			t.Errorf("sleep %d = %v, want at least %v", i, d, MinRequestInterval)
		}
	}
}

// A config may not relax the usage policy.
func TestMinIntervalIsClampedUp(t *testing.T) {
	t.Parallel()

	server := jsonServer(t, `{"address":{"village":"Musterdorf"}}`)

	var slept []time.Duration

	clock := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	client, err := NewNominatim(NominatimConfig{
		UserAgent:   "titelheld-test/1.0",
		BaseURL:     server.URL,
		HTTPClient:  server.Client(),
		MinInterval: time.Millisecond, // far below the policy
		Now:         func() time.Time { return clock },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			clock = clock.Add(d)

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewNominatim: %v", err)
	}

	for range 2 {
		if _, err := client.Reverse(t.Context(), testPoint); err != nil {
			t.Fatalf("Reverse: %v", err)
		}
	}

	if len(slept) != 1 || slept[0] < MinRequestInterval {
		t.Errorf("slept = %v, want a single wait of at least %v", slept, MinRequestInterval)
	}
}

func TestUserAgentIsRequiredAndSent(t *testing.T) {
	t.Parallel()

	if _, err := NewNominatim(NominatimConfig{}); !errors.Is(err, ErrNoUserAgent) {
		t.Errorf("NewNominatim without a User-Agent = %v, want ErrNoUserAgent", err)
	}
	if _, err := NewNominatim(NominatimConfig{UserAgent: "   "}); !errors.Is(err, ErrNoUserAgent) {
		t.Errorf("NewNominatim with a blank User-Agent = %v, want ErrNoUserAgent", err)
	}

	var gotAgent atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent.Store(r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"address":{"village":"Musterdorf"}}`))
	}))
	defer server.Close()

	client := newNominatim(t, server)

	if _, err := client.Reverse(t.Context(), testPoint); err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	if got := gotAgent.Load(); got != "titelheld-test/1.0 (test@example.invalid)" {
		t.Errorf("User-Agent = %v", got)
	}
}

func TestReverseRequestShape(t *testing.T) {
	t.Parallel()

	var gotQuery atomic.Value

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.Query().Encode())
		_, _ = w.Write([]byte(`{"address":{"village":"Musterdorf"}}`))
	}))
	defer server.Close()

	client := newNominatim(t, server)

	if _, err := client.Reverse(t.Context(), Point{Lat: 48.123456, Lon: 12.654321}); err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	query, _ := gotQuery.Load().(string)
	for _, want := range []string{"format=jsonv2", "lat=48.123456", "lon=12.654321", "addressdetails=1"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
}

func TestReverseErrors(t *testing.T) {
	t.Parallel()

	t.Run("non-200", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()

		if _, err := newNominatim(t, server).Reverse(t.Context(), testPoint); err == nil {
			t.Error("Reverse on a 429 = nil error, want error")
		}
	})

	t.Run("bad json", func(t *testing.T) {
		t.Parallel()

		if _, err := newNominatim(t, jsonServer(t, `{`)).Reverse(t.Context(), testPoint); err == nil {
			t.Error("Reverse on truncated JSON = nil error, want error")
		}
	})

	t.Run("nominatim error field is an empty place, not a failure", func(t *testing.T) {
		t.Parallel()

		place, err := newNominatim(t, jsonServer(t, `{"error":"Unable to geocode"}`)).
			Reverse(t.Context(), testPoint)
		if err != nil {
			t.Fatalf("Reverse: %v", err)
		}

		if !place.Empty() {
			t.Errorf("place = %+v, want empty", place)
		}
	})

	t.Run("transport failure", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := newNominatim(t, server)
		server.Close()

		if _, err := client.Reverse(t.Context(), testPoint); err == nil {
			t.Error("Reverse against a closed server = nil error, want error")
		}
	})

	t.Run("cancelled context stops the wait", func(t *testing.T) {
		t.Parallel()

		server := jsonServer(t, `{"address":{"village":"Musterdorf"}}`)

		// The clock is pinned so the second call always owes a wait. On the
		// real clock a stalled runner could put more than the interval between
		// the two calls, no wait would be needed, and the assertion would fail
		// for reasons that have nothing to do with cancellation.
		clock := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

		client, err := NewNominatim(NominatimConfig{
			UserAgent:  "titelheld-test/1.0",
			BaseURL:    server.URL,
			HTTPClient: server.Client(),
			Now:        func() time.Time { return clock },
			Sleep:      func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		})
		if err != nil {
			t.Fatalf("NewNominatim: %v", err)
		}

		if _, err := client.Reverse(t.Context(), testPoint); err != nil {
			t.Fatalf("first Reverse: %v", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := client.Reverse(ctx, testPoint); err == nil {
			t.Error("Reverse with a cancelled context = nil error, want error")
		}
	})
}

func TestNominatimDefaults(t *testing.T) {
	t.Parallel()

	client, err := NewNominatim(NominatimConfig{UserAgent: "titelheld-test/1.0"})
	if err != nil {
		t.Fatalf("NewNominatim: %v", err)
	}

	if client.baseURL != DefaultNominatimBaseURL {
		t.Errorf("baseURL = %q", client.baseURL)
	}
	if client.httpClient == nil || client.limiter.now == nil || client.limiter.sleep == nil {
		t.Error("NewNominatim left a default unset")
	}
	if client.limiter.interval != MinRequestInterval {
		t.Errorf("interval = %v", client.limiter.interval)
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Error("sleepContext with a cancelled context = nil, want error")
	}
	if err := sleepContext(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleepContext = %v", err)
	}
}

// The naming fallback fires only when no settlement resolved — the rural,
// isolated-start case where a gym or a solitary dwelling is the only named
// object Nominatim has. That is precisely where it must not name anything.
func TestNaturalFallbackRejectsPlacesAndLeisure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "a gym is not a natural feature",
			body: `{"category":"leisure","type":"fitness_centre","name":"Fitness Musterstadt",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "a sports centre is not a natural feature",
			body: `{"category":"leisure","type":"sports_centre","name":"Sportzentrum Muster",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "a swimming pool is not a natural feature",
			body: `{"category":"leisure","type":"swimming_pool","name":"Musterbad",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "a solitary dwelling is where the athlete lives",
			body: `{"category":"place","type":"isolated_dwelling","name":"Musterhof",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "a farm is a dwelling too",
			body: `{"category":"place","type":"farm","name":"Musterbauernhof",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "a house is the front door",
			body: `{"category":"place","type":"house","name":"Haus Muster",
				"address":{"country":"Testland"}}`,
		},
		{
			name: "an unknown natural type is not trusted",
			body: `{"category":"natural","type":"tree_of_unknown_kind","name":"Musterbaum",
				"address":{"country":"Testland"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			place, err := newNominatim(t, jsonServer(t, tt.body)).Reverse(t.Context(), testPoint)
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}

			if place.Name != "" {
				t.Errorf("Name = %q, want empty — this is not a natural feature", place.Name)
			}
			if place.Country != "Testland" {
				t.Errorf("Country = %q, want the country to survive", place.Country)
			}
		})
	}
}

// The features that are allowed keep working, and Kind is always one of our own
// constants rather than text the server chose.
func TestNaturalFeaturesThatAreAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		body     string
		wantName string
		wantKind string
	}{
		{
			body:     `{"category":"waterway","type":"river","name":"Musterfluss","address":{}}`,
			wantName: "Musterfluss", wantKind: "river",
		},
		{
			body:     `{"category":"natural","type":"peak","name":"Musterberg","address":{}}`,
			wantName: "Musterberg", wantKind: "peak",
		},
		{
			body:     `{"category":"natural","type":"water","name":"Mustersee","address":{}}`,
			wantName: "Mustersee", wantKind: "water",
		},
	}

	for _, tt := range tests {
		t.Run(tt.wantKind, func(t *testing.T) {
			t.Parallel()

			place, err := newNominatim(t, jsonServer(t, tt.body)).Reverse(t.Context(), testPoint)
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}

			if place.Name != tt.wantName || place.Kind != tt.wantKind {
				t.Errorf("place = %+v, want %s/%s", place, tt.wantName, tt.wantKind)
			}
		})
	}
}

// A throttle resolves by waiting; a block does not. They surface as different
// errors so a ban is visible rather than showing up as activities quietly
// going unnamed.
func TestThrottleAndBlockAreDistinct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status  int
		wantErr error
	}{
		{status: http.StatusTooManyRequests, wantErr: ErrThrottled},
		{status: http.StatusForbidden, wantErr: ErrBlocked},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer server.Close()

			_, err := newNominatim(t, server).Reverse(t.Context(), testPoint)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// Nominatim reports errors in more than one shape and does not document which
// appears where, so both are accepted: a shape mismatch would otherwise fail
// the decode and turn "nothing here" into an aborted route.
func TestErrorFieldShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "string", body: `{"error":"Unable to geocode"}`},
		{name: "object", body: `{"error":{"code":400,"message":"Bad request"}}`},
		{name: "unexpected shape", body: `{"error":[1,2,3]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			place, err := newNominatim(t, jsonServer(t, tt.body)).Reverse(t.Context(), testPoint)
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}

			if !place.Empty() {
				t.Errorf("place = %+v, want empty", place)
			}
		})
	}

	// An absent error field is not an error.
	place, err := newNominatim(t, jsonServer(t, `{"address":{"village":"Musterdorf"}}`)).
		Reverse(t.Context(), testPoint)
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}

	if place.Name != "Musterdorf" {
		t.Errorf("place = %+v, want Musterdorf", place)
	}
}

// An endpoint that never stops sending must not be decoded without a bound.
func TestOversizedResponseIsBounded(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"address":{"village":"`))

		// Far past the cap; the decode is cut short mid-string.
		chunk := strings.Repeat("x", 64<<10)
		for range 8 {
			_, _ = w.Write([]byte(chunk))
		}

		_, _ = w.Write([]byte(`"}}`))
	}))
	defer server.Close()

	if _, err := newNominatim(t, server).Reverse(t.Context(), testPoint); err == nil {
		t.Error("Reverse on an oversized body = nil error, want a decode failure")
	}
}
