package store

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jkreileder/titelheld/internal/strava"
)

// Memory is an in-process implementation of every store interface.
//
// It backs local runs and tests. The Firestore implementation lands with the
// store phase and must satisfy the same interfaces; nothing outside this
// package may depend on the concrete type.
//
// State is lost on restart. For the queue and the named log that is tolerable —
// both are re-derivable from Strava, and a missed activity simply keeps its
// default title. For the token pair it is not, which is exactly why the token
// pair is the one thing that has to move to Firestore.
type Memory struct {
	mu      sync.RWMutex
	tokens  map[int64]strava.Token
	pending map[key]Pending
	named   map[key]NamedTitle
	places  map[string]Place

	// franchises is keyed by athlete and franchise name, so two athletes walk
	// the same series independently.
	franchises map[franchiseKey]int

	// routes counts how often each athlete has ridden each fingerprint.
	routes map[routeKey]Route
}

// routeKey identifies one athlete's history of one route.
type routeKey struct {
	athleteID   int64
	fingerprint string
}

// franchiseKey identifies one athlete's position in one franchise.
type franchiseKey struct {
	athleteID int64
	franchise string
}

type key struct {
	athleteID  int64
	activityID int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		tokens:     make(map[int64]strava.Token),
		pending:    make(map[key]Pending),
		named:      make(map[key]NamedTitle),
		places:     make(map[string]Place),
		franchises: make(map[franchiseKey]int),
		routes:     make(map[routeKey]Route),
	}
}

// Load implements [strava.TokenStore].
func (m *Memory) Load(_ context.Context, athleteID int64) (strava.Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	token, ok := m.tokens[athleteID]
	if !ok {
		return strava.Token{}, strava.ErrTokenNotFound
	}

	return token, nil
}

// Save implements [strava.TokenStore].
func (m *Memory) Save(_ context.Context, token strava.Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tokens[token.AthleteID] = token

	return nil
}

// AnyToken returns the single stored token, if there is exactly one.
//
// The service is single-athlete, so the bootstrap can discover which athlete
// authorized without the ID being configured up front.
func (m *Memory) AnyToken(_ context.Context) (strava.Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var only strava.Token

	if len(m.tokens) != 1 {
		return only, ErrNotFound
	}

	for _, token := range m.tokens {
		only = token
	}

	return only, nil
}

// Enqueue implements [Queue].
func (m *Memory) Enqueue(_ context.Context, pending Pending) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := key{athleteID: pending.AthleteID, activityID: pending.ActivityID}
	if _, exists := m.pending[id]; exists {
		return false, nil
	}

	m.pending[id] = pending

	return true, nil
}

// Due implements [Queue].
func (m *Memory) Due(_ context.Context, now time.Time) ([]Pending, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	due := make([]Pending, 0, len(m.pending))

	for _, pending := range m.pending {
		if pending.Due(now) {
			due = append(due, pending)
		}
	}

	SortPending(due)

	return due, nil
}

// SortPending orders pending entries the way every implementation must return
// them: oldest deadline first, ties broken numerically on the activity ID.
//
// Exported because Firestore has to apply it too — its own tie-break compares
// document IDs as strings, which orders 1000 before 200.
func SortPending(due []Pending) {
	slices.SortFunc(due, byDeadline)
}

// byDeadline orders pending entries oldest deadline first, falling back to the
// activity ID so a sweep processes identical deadlines in a stable order.
func byDeadline(a, b Pending) int {
	if a.ProcessAfter.Equal(b.ProcessAfter) {
		return cmp.Compare(a.ActivityID, b.ActivityID)
	}

	if a.ProcessAfter.Before(b.ProcessAfter) {
		return -1
	}

	return 1
}

// Remove implements [Queue].
func (m *Memory) Remove(_ context.Context, athleteID, activityID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.pending, key{athleteID: athleteID, activityID: activityID})

	return nil
}

// Len implements [Queue].
func (m *Memory) Len(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.pending), nil
}

// MarkNamed implements [NamedLog].
func (m *Memory) MarkNamed(_ context.Context, naming Naming) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.named[key{athleteID: naming.AthleteID, activityID: naming.ActivityID}] = NamedTitle{
		ActivityID: naming.ActivityID,
		Title:      naming.Title,
		Language:   naming.Language,
		NamedAt:    naming.At.UTC(),
	}

	return nil
}

// RecentTitles returns the newest titles first.
//
// Ties on the timestamp break by activity ID, descending, so the order is
// total rather than merely sorted. Two activities named in the same sweep
// share a clock reading, and a test that could not say which came first would
// be asserting on map iteration order.
func (m *Memory) RecentTitles(_ context.Context, athleteID int64, limit int) ([]NamedTitle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 {
		return nil, nil
	}

	titles := make([]NamedTitle, 0, len(m.named))

	for id, entry := range m.named {
		if id.athleteID == athleteID {
			titles = append(titles, entry)
		}
	}

	slices.SortFunc(titles, func(a, b NamedTitle) int {
		if order := b.NamedAt.Compare(a.NamedAt); order != 0 {
			return order
		}

		return cmp.Compare(b.ActivityID, a.ActivityID)
	})

	if len(titles) > limit {
		titles = titles[:limit]
	}

	return titles, nil
}

// Named implements [NamedLog].
func (m *Memory) Named(_ context.Context, athleteID, activityID int64) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, ok := m.named[key{athleteID: athleteID, activityID: activityID}]

	return entry.Title, ok, nil
}

// Place implements [GeocodeCache].
func (m *Memory) Place(_ context.Context, key string) (Place, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	place, ok := m.places[key]

	return place, ok, nil
}

// SavePlace implements [GeocodeCache].
func (m *Memory) SavePlace(_ context.Context, key string, place Place) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.places[key] = place

	return nil
}

// FranchisePosition returns how many entries of the franchise are used.
func (m *Memory) FranchisePosition(_ context.Context, athleteID int64, franchise string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.franchises[franchiseKey{athleteID: athleteID, franchise: franchise}], nil
}

// AdvanceFranchise moves one entry along and returns the new position.
func (m *Memory) AdvanceFranchise(_ context.Context, athleteID int64, franchise string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := franchiseKey{athleteID: athleteID, franchise: franchise}
	m.franchises[k]++

	return m.franchises[k], nil
}

// Route returns how often the athlete has ridden this route.
func (m *Memory) Route(_ context.Context, athleteID int64, fingerprint string) (Route, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	route, ok := m.routes[routeKey{athleteID: athleteID, fingerprint: fingerprint}]

	return route, ok, nil
}

// RecordRoute counts one more ride of this route.
func (m *Memory) RecordRoute(
	_ context.Context, athleteID int64, fingerprint string, at time.Time,
) (Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := routeKey{athleteID: athleteID, fingerprint: fingerprint}

	at = at.UTC()

	route, ok := m.routes[id]
	if !ok {
		route = Route{FirstSeen: at, LastSeen: at}
	}

	// The earliest and latest ride, not the first and last recorded. Rides do
	// not arrive in the order they happened: an activity uploaded days late is
	// processed after more recent ones, and a history import would arrive in
	// whatever order it reads. "Same route as" names the earliest, so taking
	// the first one recorded would name the wrong day.
	if at.Before(route.FirstSeen) {
		route.FirstSeen = at
	}

	if at.After(route.LastSeen) {
		route.LastSeen = at
	}

	route.Count++
	m.routes[id] = route

	return route, nil
}
