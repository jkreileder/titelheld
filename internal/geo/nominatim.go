package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jkreileder/titelheld/internal/store"
)

// DefaultNominatimBaseURL is the public Nominatim endpoint.
const DefaultNominatimBaseURL = "https://nominatim.openstreetmap.org"

// MinRequestInterval is the floor Nominatim's usage policy puts on request
// rate: at most one per second, absolute.
const MinRequestInterval = time.Second

// ErrNoUserAgent means the caller did not supply the identifying User-Agent
// Nominatim's usage policy requires. Refusing to start is deliberate — an
// anonymous client is the thing that gets an IP blocked.
var ErrNoUserAgent = errors.New("geo: a Nominatim User-Agent is required")

// addressFields are the only Nominatim address keys that may reach a title,
// most specific first.
//
// This list is the privacy rule in code. Nominatim happily reports the amenity,
// shop, office or healthcare facility nearest a coordinate, and a title naming
// the athlete's doctor is exactly what must never happen. `road` and
// `house_number` are excluded for the same reason: rides start at a front door.
var addressFields = []struct {
	key  string
	kind string
}{
	{key: "village", kind: "village"},
	{key: "hamlet", kind: "hamlet"},
	{key: "town", kind: "town"},
	{key: "city", kind: "city"},
	{key: "municipality", kind: "municipality"},
	{key: "suburb", kind: "suburb"},
	{key: "city_district", kind: "district"},
	{key: "county", kind: "county"},
}

// regionFields name the coarser container, most specific first.
var regionFields = []string{"state_district", "state", "region", "province"}

// naturalCategories are Nominatim categories whose name may be used directly:
// rivers, lakes, forests, hills. A route along the Donau should be able to say
// so.
var naturalCategories = map[string]string{
	"waterway": "waterway",
	"natural":  "natural",
	"leisure":  "",
	"place":    "place",
}

// reverseResponse is the subset of Nominatim's jsonv2 reply that is read.
//
// Everything omitted is omitted on purpose: display_name concatenates the whole
// address including the house number and the nearest POI.
type reverseResponse struct {
	Category string            `json:"category"`
	Type     string            `json:"type"`
	Name     string            `json:"name"`
	Address  map[string]string `json:"address"`
	Error    string            `json:"error"`
}

// limiter enforces a minimum interval between requests.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) error
}

// wait blocks until the next request is allowed.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	if !l.last.IsZero() {
		if elapsed := now.Sub(l.last); elapsed < l.interval {
			if err := l.sleep(ctx, l.interval-elapsed); err != nil {
				return err
			}

			now = l.now()
		}
	}

	l.last = now

	return nil
}

// Nominatim reverse-geocodes coordinates into verified place names.
type Nominatim struct {
	baseURL    string
	userAgent  string
	httpClient *http.Client
	limiter    *limiter
}

// NominatimConfig configures a [Nominatim] client.
type NominatimConfig struct {
	// UserAgent identifies this service to Nominatim. Required by their usage
	// policy, and required here.
	UserAgent string

	// BaseURL defaults to [DefaultNominatimBaseURL].
	BaseURL string

	// HTTPClient defaults to a client with a 20 second timeout.
	HTTPClient *http.Client

	// MinInterval defaults to [MinRequestInterval]. It is clamped up to that
	// value: the usage policy is not something a config file may relax.
	MinInterval time.Duration

	// Now and Sleep are injected so the rate limit is testable without
	// spending real seconds.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewNominatim builds a reverse-geocoding client.
func NewNominatim(cfg NominatimConfig) (*Nominatim, error) {
	if strings.TrimSpace(cfg.UserAgent) == "" {
		return nil, ErrNoUserAgent
	}

	interval := cfg.MinInterval
	if interval < MinRequestInterval {
		interval = MinRequestInterval
	}

	client := &Nominatim{
		baseURL:    strings.TrimSuffix(cfg.BaseURL, "/"),
		userAgent:  cfg.UserAgent,
		httpClient: cfg.HTTPClient,
		limiter: &limiter{
			interval: interval,
			now:      cfg.Now,
			sleep:    cfg.Sleep,
		},
	}

	if client.baseURL == "" {
		client.baseURL = DefaultNominatimBaseURL
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	if client.limiter.now == nil {
		client.limiter.now = time.Now
	}
	if client.limiter.sleep == nil {
		client.limiter.sleep = sleepContext
	}

	return client, nil
}

// Reverse resolves one coordinate into a [store.Place].
//
// It waits out the rate limit first, so callers cannot exceed the policy by
// looping.
func (n *Nominatim) Reverse(ctx context.Context, point Point) (store.Place, error) {
	if err := n.limiter.wait(ctx); err != nil {
		return store.Place{}, err
	}

	query := url.Values{
		"format": {"jsonv2"},
		"lat":    {strconv.FormatFloat(point.Lat, 'f', 6, 64)},
		"lon":    {strconv.FormatFloat(point.Lon, 'f', 6, 64)},
		// zoom 12 is roughly "town"; it keeps Nominatim from resolving to a
		// building or a shop in the first place.
		"zoom":            {"12"},
		"addressdetails":  {"1"},
		"namedetails":     {"0"},
		"extratags":       {"0"},
		"accept-language": {"de,en"},
	}

	endpoint := n.baseURL + "/reverse?" + query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return store.Place{}, fmt.Errorf("geo: build reverse request: %w", err)
	}

	request.Header.Set("User-Agent", n.userAgent)
	request.Header.Set("Accept", "application/json")

	response, err := n.httpClient.Do(request)
	if err != nil {
		return store.Place{}, fmt.Errorf("geo: reverse request: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		return store.Place{}, fmt.Errorf("geo: reverse request: unexpected status %d", response.StatusCode)
	}

	var payload reverseResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return store.Place{}, fmt.Errorf("geo: decode reverse response: %w", err)
	}

	if payload.Error != "" {
		return store.Place{}, nil
	}

	return placeFrom(payload), nil
}

// placeFrom extracts only what may safely become part of a title.
//
// Everything not on the allow-lists is dropped, including the free-text
// display_name, which is never read at all.
func placeFrom(payload reverseResponse) store.Place {
	place := store.Place{
		Country: payload.Address["country"],
	}

	for _, field := range regionFields {
		if value := payload.Address[field]; value != "" {
			place.Region = value

			break
		}
	}

	for _, field := range addressFields {
		if value := payload.Address[field.key]; value != "" {
			place.Name = value
			place.Kind = field.kind

			break
		}
	}

	// A named natural feature — a river, a lake, a forest — is worth having and
	// reveals nothing about the athlete. It is only trusted when the category
	// says so; a name attached to an amenity or a shop is discarded.
	if place.Name == "" && payload.Name != "" {
		if kind, ok := naturalCategories[payload.Category]; ok {
			place.Name = payload.Name

			if kind == "" {
				kind = payload.Type
			}

			place.Kind = kind
		}
	}

	return place
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
