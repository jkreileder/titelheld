// Package firestore is the Firestore-backed implementation of the store
// interfaces.
//
// The OAuth token pair is the reason this package exists. Strava rotates the
// refresh token on every refresh and invalidates the previous one immediately,
// so a restart that forgets it strands the service with no way back short of
// running the authorization flow by hand. Everything else kept here — the
// queue, the named log, the geocode cache — is a convenience that could be
// re-derived from Strava or refetched.
package firestore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jkreileder/titelheld/internal/store"
	"github.com/jkreileder/titelheld/internal/strava"
)

// Collection names. Each is listed in the IAM documentation; adding one means
// updating that list.
const (
	CollectionTokens   = "tokens"
	CollectionPending  = "pending"
	CollectionNamed    = "named"
	CollectionGeocache = "geocache"
)

// Store implements [store.Store] on Firestore.
type Store struct {
	client *firestore.Client
	prefix string
}

// Config configures a [Store].
type Config struct {
	// ProjectID is the GCP project holding the database. Required.
	ProjectID string

	// Database is the Firestore database ID. Empty means "(default)"; a named
	// database is what lets the runtime service account be scoped to this data
	// and nothing else.
	Database string

	// Prefix is prepended to every collection name. Production leaves it
	// empty; tests use it to isolate one run from another.
	Prefix string

	// ClientOptions are passed through to the Firestore client.
	ClientOptions []option.ClientOption
}

// New opens a Firestore-backed store.
//
// When FIRESTORE_EMULATOR_HOST is set the client library talks to the emulator
// and ignores credentials entirely, which is how the tests and CI avoid ever
// touching a real project.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("firestore: ProjectID is required")
	}

	var (
		client *firestore.Client
		err    error
	)

	if cfg.Database == "" {
		client, err = firestore.NewClient(ctx, cfg.ProjectID, cfg.ClientOptions...)
	} else {
		client, err = firestore.NewClientWithDatabase(ctx, cfg.ProjectID, cfg.Database, cfg.ClientOptions...)
	}

	if err != nil {
		return nil, fmt.Errorf("firestore: open client: %w", err)
	}

	return &Store{client: client, prefix: cfg.Prefix}, nil
}

// Close releases the underlying client.
func (s *Store) Close() error {
	if err := s.client.Close(); err != nil {
		return fmt.Errorf("firestore: close: %w", err)
	}

	return nil
}

func (s *Store) collection(name string) *firestore.CollectionRef {
	return s.client.Collection(s.prefix + name)
}

// activityKey is the document ID for anything keyed by athlete and activity.
func activityKey(athleteID, activityID int64) string {
	return strconv.FormatInt(athleteID, 10) + "-" + strconv.FormatInt(activityID, 10)
}

// notFound reports whether err is Firestore's "no such document".
func notFound(err error) bool {
	return status.Code(err) == codes.NotFound
}

// tokenDoc is the stored shape of an OAuth token pair. Field names are explicit
// so a Go rename cannot silently orphan existing data.
type tokenDoc struct {
	AthleteID    int64     `firestore:"athlete_id"`
	AccessToken  string    `firestore:"access_token"`
	RefreshToken string    `firestore:"refresh_token"`
	ExpiresAt    time.Time `firestore:"expires_at"`
	Scopes       []string  `firestore:"scopes"`
}

// Load implements [strava.TokenStore].
func (s *Store) Load(ctx context.Context, athleteID int64) (strava.Token, error) {
	snapshot, err := s.collection(CollectionTokens).
		Doc(strconv.FormatInt(athleteID, 10)).Get(ctx)
	if err != nil {
		if notFound(err) {
			return strava.Token{}, strava.ErrTokenNotFound
		}

		return strava.Token{}, fmt.Errorf("firestore: load token: %w", err)
	}

	var doc tokenDoc
	if err := snapshot.DataTo(&doc); err != nil {
		return strava.Token{}, fmt.Errorf("firestore: decode token: %w", err)
	}

	return strava.Token{
		AthleteID:    doc.AthleteID,
		AccessToken:  doc.AccessToken,
		RefreshToken: doc.RefreshToken,
		ExpiresAt:    doc.ExpiresAt.UTC(),
		Scopes:       doc.Scopes,
	}, nil
}

// Save implements [strava.TokenStore].
//
// The write replaces the document rather than merging, so a rotated refresh
// token can never sit alongside its predecessor.
func (s *Store) Save(ctx context.Context, token strava.Token) error {
	doc := tokenDoc{
		AthleteID:    token.AthleteID,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		ExpiresAt:    token.ExpiresAt.UTC(),
		Scopes:       token.Scopes,
	}

	_, err := s.collection(CollectionTokens).
		Doc(strconv.FormatInt(token.AthleteID, 10)).Set(ctx, doc)
	if err != nil {
		return fmt.Errorf("firestore: save token: %w", err)
	}

	return nil
}

// AnyToken returns the single stored token, if there is exactly one.
func (s *Store) AnyToken(ctx context.Context) (strava.Token, error) {
	snapshots, err := s.collection(CollectionTokens).Limit(2).Documents(ctx).GetAll()
	if err != nil {
		return strava.Token{}, fmt.Errorf("firestore: list tokens: %w", err)
	}

	if len(snapshots) != 1 {
		return strava.Token{}, store.ErrNotFound
	}

	var doc tokenDoc
	if err := snapshots[0].DataTo(&doc); err != nil {
		return strava.Token{}, fmt.Errorf("firestore: decode token: %w", err)
	}

	return strava.Token{
		AthleteID:    doc.AthleteID,
		AccessToken:  doc.AccessToken,
		RefreshToken: doc.RefreshToken,
		ExpiresAt:    doc.ExpiresAt.UTC(),
		Scopes:       doc.Scopes,
	}, nil
}

// pendingDoc is the stored shape of a queued activity.
type pendingDoc struct {
	AthleteID    int64     `firestore:"athlete_id"`
	ActivityID   int64     `firestore:"activity_id"`
	Aspect       string    `firestore:"aspect"`
	EnqueuedAt   time.Time `firestore:"enqueued_at"`
	ProcessAfter time.Time `firestore:"process_after"`
}

// Enqueue implements [store.Queue].
//
// Create, not Set: it fails with AlreadyExists when the document is there, so
// the "already queued" answer comes from Firestore atomically rather than from
// a read followed by a write that could interleave.
func (s *Store) Enqueue(ctx context.Context, pending store.Pending) (bool, error) {
	doc := pendingDoc{
		AthleteID:    pending.AthleteID,
		ActivityID:   pending.ActivityID,
		Aspect:       pending.Aspect,
		EnqueuedAt:   pending.EnqueuedAt.UTC(),
		ProcessAfter: pending.ProcessAfter.UTC(),
	}

	_, err := s.collection(CollectionPending).
		Doc(activityKey(pending.AthleteID, pending.ActivityID)).Create(ctx, doc)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return false, nil
		}

		return false, fmt.Errorf("firestore: enqueue: %w", err)
	}

	return true, nil
}

// Due implements [store.Queue].
//
// The inequality and the ordering are on the same field, so Firestore serves
// this from the automatic single-field index — no composite index to create or
// keep in sync with the code.
func (s *Store) Due(ctx context.Context, now time.Time) ([]store.Pending, error) {
	snapshots, err := s.collection(CollectionPending).
		Where("process_after", "<=", now.UTC()).
		OrderBy("process_after", firestore.Asc).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, fmt.Errorf("firestore: due query: %w", err)
	}

	due := make([]store.Pending, 0, len(snapshots))

	for _, snapshot := range snapshots {
		var doc pendingDoc
		if err := snapshot.DataTo(&doc); err != nil {
			return nil, fmt.Errorf("firestore: decode pending: %w", err)
		}

		due = append(due, store.Pending{
			AthleteID:    doc.AthleteID,
			ActivityID:   doc.ActivityID,
			Aspect:       doc.Aspect,
			EnqueuedAt:   doc.EnqueuedAt.UTC(),
			ProcessAfter: doc.ProcessAfter.UTC(),
		})
	}

	return due, nil
}

// Remove implements [store.Queue]. Deleting an absent document is a no-op in
// Firestore, which matches the interface's forgiving contract.
func (s *Store) Remove(ctx context.Context, athleteID, activityID int64) error {
	_, err := s.collection(CollectionPending).Doc(activityKey(athleteID, activityID)).Delete(ctx)
	if err != nil {
		return fmt.Errorf("firestore: remove pending: %w", err)
	}

	return nil
}

// Len implements [store.Queue].
//
// The documents are counted by listing their IDs rather than with a count
// aggregation: the queue holds a handful of entries for one athlete, and this
// keeps the implementation to features the emulator is certain to support.
func (s *Store) Len(ctx context.Context) (int, error) {
	snapshots, err := s.collection(CollectionPending).
		Select().Documents(ctx).GetAll()
	if err != nil {
		return 0, fmt.Errorf("firestore: count pending: %w", err)
	}

	return len(snapshots), nil
}

// namedDoc is the stored shape of a written title.
type namedDoc struct {
	AthleteID  int64     `firestore:"athlete_id"`
	ActivityID int64     `firestore:"activity_id"`
	Title      string    `firestore:"title"`
	NamedAt    time.Time `firestore:"named_at"`
}

// MarkNamed implements [store.NamedLog].
func (s *Store) MarkNamed(ctx context.Context, athleteID, activityID int64, title string) error {
	doc := namedDoc{
		AthleteID:  athleteID,
		ActivityID: activityID,
		Title:      title,
		NamedAt:    time.Now().UTC(),
	}

	_, err := s.collection(CollectionNamed).Doc(activityKey(athleteID, activityID)).Set(ctx, doc)
	if err != nil {
		return fmt.Errorf("firestore: mark named: %w", err)
	}

	return nil
}

// Named implements [store.NamedLog].
func (s *Store) Named(ctx context.Context, athleteID, activityID int64) (string, bool, error) {
	snapshot, err := s.collection(CollectionNamed).Doc(activityKey(athleteID, activityID)).Get(ctx)
	if err != nil {
		if notFound(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("firestore: read named log: %w", err)
	}

	var doc namedDoc
	if err := snapshot.DataTo(&doc); err != nil {
		return "", false, fmt.Errorf("firestore: decode named log: %w", err)
	}

	return doc.Title, true, nil
}

// placeDoc is the stored shape of a cached place.
//
// Only names are stored. The coordinates that produced them live in the
// document ID as a rounded key and nowhere else.
type placeDoc struct {
	Name     string    `firestore:"name"`
	Kind     string    `firestore:"kind"`
	Region   string    `firestore:"region"`
	Country  string    `firestore:"country"`
	CachedAt time.Time `firestore:"cached_at"`
}

// Place implements [store.GeocodeCache].
func (s *Store) Place(ctx context.Context, key string) (store.Place, bool, error) {
	snapshot, err := s.collection(CollectionGeocache).Doc(cacheDocID(key)).Get(ctx)
	if err != nil {
		if notFound(err) {
			return store.Place{}, false, nil
		}

		return store.Place{}, false, fmt.Errorf("firestore: read geocode cache: %w", err)
	}

	var doc placeDoc
	if err := snapshot.DataTo(&doc); err != nil {
		return store.Place{}, false, fmt.Errorf("firestore: decode cached place: %w", err)
	}

	return store.Place{
		Name:    doc.Name,
		Kind:    doc.Kind,
		Region:  doc.Region,
		Country: doc.Country,
	}, true, nil
}

// SavePlace implements [store.GeocodeCache].
func (s *Store) SavePlace(ctx context.Context, key string, place store.Place) error {
	doc := placeDoc{
		Name:     place.Name,
		Kind:     place.Kind,
		Region:   place.Region,
		Country:  place.Country,
		CachedAt: time.Now().UTC(),
	}

	_, err := s.collection(CollectionGeocache).Doc(cacheDocID(key)).Set(ctx, doc)
	if err != nil {
		return fmt.Errorf("firestore: save cached place: %w", err)
	}

	return nil
}

// cacheDocID makes a geocode key safe as a Firestore document ID.
//
// A document ID may not contain a forward slash and may not be "." or "..";
// rounded coordinate keys contain neither, but the cache is keyed by a string
// this package does not own, so the substitution is done here rather than
// assumed.
func cacheDocID(key string) string {
	if key == "" {
		return "_"
	}

	safe := make([]rune, 0, len(key))

	for _, r := range key {
		if r == '/' {
			safe = append(safe, '_')

			continue
		}

		safe = append(safe, r)
	}

	id := string(safe)
	if id == "." || id == ".." {
		return "_" + id
	}

	return id
}
