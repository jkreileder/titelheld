package geo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

var (
	// ErrNoUserAgent means the caller did not supply the identifying
	// User-Agent Nominatim's usage policy requires. Refusing to start is
	// deliberate — an anonymous client is the thing that gets an IP blocked.
	ErrNoUserAgent = errors.New("geo: a Nominatim User-Agent is required")

	// ErrThrottled is a 429: the request rate was too high for the moment.
	ErrThrottled = errors.New("geo: throttled by Nominatim")

	// ErrBlocked is a 403, which for Nominatim means the address is banned
	// rather than the request being wrong. It is separate from ErrThrottled
	// because it does not resolve by waiting, and it would otherwise surface
	// only as activities quietly going unnamed.
	ErrBlocked = errors.New("geo: blocked by Nominatim")
)

// maxResponseBytes bounds the reverse-geocoding response. A few kilobytes is
// typical; this is generous and still stops a misbehaving or hostile endpoint
// from growing the decode until the process runs out of memory.
const maxResponseBytes = 256 << 10

// Zoom levels. [DefaultZoom] asks Nominatim for settlement granularity: the
// finer address fields a resolution order prefers are absent from a coarser
// response, so a hamlet cannot be preferred over a town at a zoom that never
// reports one.
//
// The zoom is not a privacy control. What keeps a road, a house number or the
// nearest surgery out of a title is [addressFields] and [naturalFeatures], and
// they apply to every response whatever it was asked for.
const (
	DefaultZoom = 16
	MinZoom     = 3
	MaxZoom     = 18
)

// defaultPlaceFields is the order a point's name is resolved in: the finest
// settlement first, and a municipality — which spans several of them — only
// where nothing finer is reported.
var defaultPlaceFields = []string{"hamlet", "village", "suburb", "town"}

// DefaultPlaceFields returns the shipped resolution order.
//
// A copy, so a caller holding it cannot reorder what every other caller
// resolves by.
func DefaultPlaceFields() []string {
	return slices.Clone(defaultPlaceFields)
}

// addressField is one allow-listed Nominatim address key and the Kind a name
// taken from it is reported as.
type addressField struct {
	key  string
	kind string
}

// addressFields are the only Nominatim address keys that may reach a title.
//
// This list is the privacy rule in code. Nominatim happily reports the amenity,
// shop, office or healthcare facility nearest a coordinate, and a title naming
// the athlete's doctor is exactly what must never happen. `road` and
// `house_number` are excluded for the same reason: rides start at a front door.
//
// This is a set, not a preference: which key wins for a point is the
// configured resolution order, and the order here only settles what is left
// over once that order is exhausted.
var addressFields = []addressField{
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

// naturalFeatures are the only (category, type) pairs whose *name* may be used
// directly, when no settlement resolved. A route along the Donau should be able
// to say so.
//
// This is an allow-list of types, not of categories, because a category is far
// too coarse to be safe. OSM's "leisure" covers fitness_centre, sports_centre,
// swimming_pool and golf_course; "place" covers isolated_dwelling — literally
// "a solitary dwelling", which on a rural route is the athlete's house and the
// only named object Nominatim has to offer. Admitting either category would
// make the naming fallback fire exactly where it is most dangerous.
//
// The value is the Kind reported, so Kind is always one of these fixed strings
// and never text the server chose.
var naturalFeatures = map[string]map[string]string{
	"waterway": {
		"river":  "river",
		"stream": "stream",
		"canal":  "canal",
	},
	"natural": {
		"water":     "water",
		"bay":       "bay",
		"strait":    "strait",
		"wood":      "wood",
		"forest":    "forest",
		"heath":     "heath",
		"moor":      "moor",
		"grassland": "grassland",
		"beach":     "beach",
		"cliff":     "cliff",
		"peak":      "peak",
		"ridge":     "ridge",
		"valley":    "valley",
		"saddle":    "saddle",
		"glacier":   "glacier",
	},
}

// allowedKinds is every Kind placeFrom can set, and nothing else.
//
// placeFrom sets Name and Kind together and never one without the other, so a
// place whose Kind is not in here did not come from the allow-list. That is
// what makes the read-side check in [Describer.resolve] possible: it does not
// need the original payload, only the claim the Kind makes about where the
// name came from.
var allowedKinds = func() map[string]struct{} {
	kinds := make(map[string]struct{}, len(addressFields))

	for _, field := range addressFields {
		kinds[field.kind] = struct{}{}
	}

	for _, types := range naturalFeatures {
		for _, kind := range types {
			kinds[kind] = struct{}{}
		}
	}

	return kinds
}()

// IsAllowedKind reports whether a place name of this kind may become part of a
// title.
func IsAllowedKind(kind string) bool {
	_, ok := allowedKinds[kind]

	return ok
}

// IsPlaceField reports whether a Nominatim address key may appear in a
// resolution order. Configuration validates against this rather than against
// its own copy of the names, so a key this package cannot read is refused
// where it is set instead of being silently skipped where it is used.
func IsPlaceField(key string) bool {
	return slices.ContainsFunc(addressFields, func(field addressField) bool {
		return field.key == key
	})
}

// PlaceFields returns every address key that may appear in a resolution
// order, so an error message can name the set it rejected against.
func PlaceFields() []string {
	keys := make([]string, 0, len(addressFields))

	for _, field := range addressFields {
		keys = append(keys, field.key)
	}

	return keys
}

// orderFields resolves a configured order into the full sequence a name is
// looked up in: the configured keys first, then everything the allow-list
// carries that the order did not name.
//
// The remainder is appended rather than dropped, so a point that reports only
// a coarse container still gets a name; it comes last, so a coarse name can
// never win over one the order asked for.
func orderFields(order []string) ([]addressField, error) {
	if len(order) == 0 {
		order = defaultPlaceFields
	}

	fields := make([]addressField, 0, len(addressFields))
	taken := make(map[string]struct{}, len(addressFields))

	for _, key := range order {
		index := slices.IndexFunc(addressFields, func(field addressField) bool {
			return field.key == key
		})

		if index < 0 {
			return nil, fmt.Errorf("geo: %q is not an address key this service reads", key)
		}

		if _, ok := taken[key]; ok {
			return nil, fmt.Errorf("geo: address key %q is listed twice", key)
		}

		taken[key] = struct{}{}
		fields = append(fields, addressFields[index])
	}

	for _, field := range addressFields {
		if _, ok := taken[field.key]; !ok {
			fields = append(fields, field)
		}
	}

	return fields, nil
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
	Error    nominatimError    `json:"error"`
}

// nominatimError accepts both shapes the error field is reported in.
//
// Nominatim answers an unresolvable coordinate with {"error":"Unable to
// geocode"}, and some failures with an object carrying a code and a message.
// Which one appears where is not documented, so both are accepted rather than
// betting on one: a shape mismatch would otherwise fail the whole decode and
// turn "nothing here" into an aborted route.
type nominatimError struct {
	message string
	present bool
}

func (e *nominatimError) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		e.message = text
		e.present = text != ""

		return nil
	}

	// Whatever shape it is, the field being present at all means Nominatim had
	// nothing to return. Only the message is best-effort.
	e.present = true

	var object struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(data, &object); err == nil {
		e.message = object.Message
	}

	return nil
}

// reported says whether the response carried an error at all.
func (e nominatimError) reported() bool {
	return e.present
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
	baseURL     string
	userAgent   string
	httpClient  *http.Client
	limiter     *limiter
	zoom        int
	placeFields []addressField
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

	// Zoom is the granularity every reverse request asks for. Zero means
	// [DefaultZoom]; anything outside [MinZoom] to [MaxZoom] is refused.
	Zoom int

	// PlaceFields is the order a point's name is resolved in. Empty means
	// [DefaultPlaceFields]. Every entry must be an address key this package
	// reads, and no key may appear twice.
	PlaceFields []string

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

	zoom := cfg.Zoom
	if zoom == 0 {
		zoom = DefaultZoom
	}

	if zoom < MinZoom || zoom > MaxZoom {
		return nil, fmt.Errorf("geo: zoom %d is outside %d to %d", zoom, MinZoom, MaxZoom)
	}

	fields, err := orderFields(cfg.PlaceFields)
	if err != nil {
		return nil, err
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
		zoom:        zoom,
		placeFields: fields,
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
		"format":          {"jsonv2"},
		"lat":             {strconv.FormatFloat(point.Lat, 'f', 6, 64)},
		"lon":             {strconv.FormatFloat(point.Lon, 'f', 6, 64)},
		"zoom":            {strconv.Itoa(n.zoom)},
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

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return store.Place{}, ErrThrottled
	case http.StatusForbidden:
		return store.Place{}, ErrBlocked
	default:
		return store.Place{}, fmt.Errorf("geo: reverse request: unexpected status %d", response.StatusCode)
	}

	var payload reverseResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return store.Place{}, fmt.Errorf("geo: decode reverse response: %w", err)
	}

	// "Nothing here" is an answer, not a failure: Nominatim has nothing to say
	// about the middle of a lake.
	if payload.Error.reported() {
		return store.Place{}, nil
	}

	return n.placeFrom(payload), nil
}

// placeFrom extracts only what may safely become part of a title.
//
// Everything not on the allow-lists is dropped, including the free-text
// display_name, which is never read at all.
//
// The name is the first key of the resolution order this point reports, so a
// hamlet inside a town is named as the hamlet, and the municipality several
// villages share is reached only where the point has nothing finer to offer.
func (n *Nominatim) placeFrom(payload reverseResponse) store.Place {
	place := store.Place{
		Country: payload.Address["country"],
	}

	for _, field := range regionFields {
		if value := payload.Address[field]; value != "" {
			place.Region = value

			break
		}
	}

	for _, field := range n.placeFields {
		if value := payload.Address[field.key]; value != "" {
			place.Name = value
			place.Kind = field.kind

			break
		}
	}

	// A named natural feature — a river, a lake, a ridge — is worth having and
	// reveals nothing about the athlete. It is trusted only when both the
	// category and the type are on the allow-list; a name attached to anything
	// else, including a gym or a solitary dwelling, is discarded.
	if place.Name == "" && payload.Name != "" {
		if types, ok := naturalFeatures[payload.Category]; ok {
			if kind, ok := types[payload.Type]; ok {
				place.Name = payload.Name
				place.Kind = kind
			}
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
