package store

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jkreileder/strava-namer/internal/strava"
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
	named   map[key]string
}

type key struct {
	athleteID  int64
	activityID int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		tokens:  make(map[int64]strava.Token),
		pending: make(map[key]Pending),
		named:   make(map[key]string),
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

	slices.SortFunc(due, byDeadline)

	return due, nil
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
func (m *Memory) MarkNamed(_ context.Context, athleteID, activityID int64, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.named[key{athleteID: athleteID, activityID: activityID}] = title

	return nil
}

// Named implements [NamedLog].
func (m *Memory) Named(_ context.Context, athleteID, activityID int64) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	title, ok := m.named[key{athleteID: athleteID, activityID: activityID}]

	return title, ok, nil
}
