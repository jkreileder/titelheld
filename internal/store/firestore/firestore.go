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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	CollectionTokens    = "tokens"
	CollectionPending   = "pending"
	CollectionNamed     = "named"
	CollectionGeocache  = "geocache"
	CollectionFranchise = "franchise"
	CollectionConfig    = "config"
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
//
// The result is re-sorted in memory before it is returned. Firestore breaks
// ties on the document ID as a *string*, so two events queued in the same
// second would come back as ["1-1000", "1-200"] while the in-memory store
// orders them numerically. Sorting here costs nothing at this size and is what
// makes "same semantics" true for ties as well as for deadlines; the
// alternative, a secondary OrderBy, would need a composite index.
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

	store.SortPending(due)

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
	Language   string    `firestore:"language"`
	Source     string    `firestore:"source"`
	NamedAt    time.Time `firestore:"named_at"`
}

// MarkNamed implements [store.NamedLog].
func (s *Store) MarkNamed(ctx context.Context, naming store.Naming) error {
	doc := namedDoc{
		AthleteID:  naming.AthleteID,
		ActivityID: naming.ActivityID,
		Title:      naming.Title,
		Language:   naming.Language,
		Source:     naming.Source,
		NamedAt:    naming.At.UTC(),
	}

	_, err := s.collection(CollectionNamed).
		Doc(activityKey(naming.AthleteID, naming.ActivityID)).Set(ctx, doc)
	if err != nil {
		return fmt.Errorf("firestore: mark named: %w", err)
	}

	return nil
}

// RecentTitles returns the newest titles first.
//
// This is the one query in this package that needs a composite index: an
// equality on athlete_id with an ordering on named_at. Terraform declares it,
// and the deploy order in the README puts the apply before the sweep is
// unpaused, because a missing index is a runtime error on every naming rather
// than something that degrades quietly.
//
// The emulator will not tell you if the index is wrong. It serves any query
// without one, so a mismatch between the declaration and this query passes
// every test here and fails only in production.
func (s *Store) RecentTitles(
	ctx context.Context, athleteID int64, limit int,
) ([]store.NamedTitle, error) {
	if limit <= 0 {
		return nil, nil
	}

	snapshots, err := s.collection(CollectionNamed).
		Where("athlete_id", "==", athleteID).
		OrderBy("named_at", firestore.Desc).
		Limit(limit).
		Documents(ctx).GetAll()
	if err != nil {
		return nil, fmt.Errorf("firestore: read recent titles: %w", err)
	}

	titles := make([]store.NamedTitle, 0, len(snapshots))

	for _, snapshot := range snapshots {
		var doc namedDoc
		if err := snapshot.DataTo(&doc); err != nil {
			return nil, fmt.Errorf("firestore: decode recent title: %w", err)
		}

		titles = append(titles, store.NamedTitle{
			ActivityID: doc.ActivityID,
			Title:      doc.Title,
			Language:   doc.Language,
			Source:     doc.Source,
			NamedAt:    doc.NamedAt.UTC(),
		})
	}

	return titles, nil
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
// A document ID may not contain a forward slash, may not be "." or "..", and
// may not exceed 1500 bytes. Rounded coordinate keys satisfy all of that, but
// the cache is keyed by a string this package does not own.
//
// The mapping must be injective, not merely safe: substituting one character
// for another would let two distinct keys land on one document and share a
// cached place — two different points reported as the same village. Anything
// not plainly safe is therefore encoded whole, under a prefix the safe set
// cannot produce.
func cacheDocID(key string) string {
	if isSafeDocID(key) {
		return key
	}

	// Hashed rather than encoded: an encoding of an over-long key is longer
	// still, and Firestore's 1500-byte limit applies to the result. A digest is
	// fixed width and collision-resistant, which is what injectivity needs in
	// practice.
	digest := sha256.Sum256([]byte(key))

	return docIDEscapePrefix + hex.EncodeToString(digest[:])
}

// docIDEscapePrefix marks a hashed document ID. "=" is valid in a document ID
// and is excluded from the safe set below, so no unhashed key can begin with
// it.
const docIDEscapePrefix = "="

// maxDocIDBytes is Firestore's document ID limit.
const maxDocIDBytes = 1500

func isSafeDocID(key string) bool {
	if key == "" || key == "." || key == ".." || len(key) > maxDocIDBytes {
		return false
	}

	// Firestore reserves IDs matching __.*__ and rejects them outright. The
	// safe set below accepts "_", so this has to be excluded explicitly or a
	// key like __proto__ would be passed straight through and fail the write.
	if strings.HasPrefix(key, "__") && strings.HasSuffix(key, "__") && len(key) >= 4 {
		return false
	}

	for _, r := range key {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
		case r == '.' || r == ',' || r == '-' || r == '_' || r == '~':
		default:
			return false
		}
	}

	return true
}

// franchiseDoc is one athlete's position in one franchise.
//
// The titles are not stored: they are configuration, and a franchise renamed
// or reordered in config must not require migrating anything here.
type franchiseDoc struct {
	AthleteID int64     `firestore:"athlete_id"`
	Franchise string    `firestore:"franchise"`
	Position  int       `firestore:"position"`
	UpdatedAt time.Time `firestore:"updated_at"`
}

// franchiseKey is the document ID for one athlete's position in one franchise.
//
// The franchise name is configuration, so it is a string this package does not
// own and cannot assume anything about. The one this service ships with is
// "Pink Panther" — a space, which the safe set rejects — and a name containing
// "/" would not be a document ID at all but a path with the wrong number of
// segments. So the composed key goes through the same escaping the geocode
// cache uses, and for the same reason: the mapping has to stay injective, or
// two franchises would share one position and hand out the same title twice.
func franchiseKey(athleteID int64, franchise string) string {
	return cacheDocID(strconv.FormatInt(athleteID, 10) + "-" + franchise)
}

// FranchisePosition returns the index the franchise's rotation resumes at.
//
// A franchise never used, and one that no longer exists in configuration, both
// answer zero: removing a franchise from config should stop it being consulted,
// not start producing errors.
func (s *Store) FranchisePosition(ctx context.Context, athleteID int64, franchise string) (int, error) {
	snapshot, err := s.collection(CollectionFranchise).Doc(franchiseKey(athleteID, franchise)).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return 0, nil
		}

		return 0, fmt.Errorf("firestore: franchise position: %w", err)
	}

	var doc franchiseDoc
	if err := snapshot.DataTo(&doc); err != nil {
		return 0, fmt.Errorf("firestore: decode franchise position: %w", err)
	}

	return doc.Position, nil
}

// AdvanceFranchisePast moves the position past a used entry and returns it.
//
// The move happens inside a transaction so the store decides the next number
// rather than the caller: two callers reading and writing separately would
// both land on the same position and reuse a title.
//
// Monotonic. An index below the stored position leaves it alone — a replayed
// or out-of-order naming must not hand a spent entry out again — which is
// also why this takes an index rather than setting whatever it is told.
func (s *Store) AdvanceFranchisePast(
	ctx context.Context, athleteID int64, franchise string, index int,
) (int, error) {
	if index < 0 {
		return 0, fmt.Errorf("firestore: advance franchise %q: index %d is negative", franchise, index)
	}

	ref := s.collection(CollectionFranchise).Doc(franchiseKey(athleteID, franchise))

	var position int

	err := s.client.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		position = 0

		snapshot, err := tx.Get(ref)
		switch {
		case err != nil && status.Code(err) != codes.NotFound:
			return err
		case err == nil:
			var doc franchiseDoc
			if err := snapshot.DataTo(&doc); err != nil {
				return err
			}

			position = doc.Position
		}

		position = max(position, index+1)

		return tx.Set(ref, franchiseDoc{
			AthleteID: athleteID,
			Franchise: franchise,
			Position:  position,
			UpdatedAt: time.Now().UTC(),
		})
	})
	if err != nil {
		return 0, fmt.Errorf("firestore: advance franchise: %w", err)
	}

	return position, nil
}

// configDoc is the per-athlete configuration document.
//
// The stored shape is this package's, not the naming layer's, so a change to
// how a franchise is represented in code is a deliberate decision here rather
// than a silent schema change on disk.
type configDoc struct {
	AthleteID  int64            `firestore:"athlete_id"`
	Franchises []franchiseEntry `firestore:"franchises"`
	UpdatedAt  time.Time        `firestore:"updated_at"`
}

type franchiseEntry struct {
	Name       string   `firestore:"name"`
	SportTypes []string `firestore:"sport_types"`
	GearName   string   `firestore:"gear_name"`
	Titles     []string `firestore:"titles"`

	// Reserved names entries of Titles the rotation never offers. A separate
	// field rather than a shape change to Titles: the collection already holds
	// a document, and an athlete who has not written this field reads back as
	// nil, which is "nothing reserved" — the behavior before it existed.
	Reserved []string `firestore:"reserved"`
}

// AthleteConfig returns the athlete's configuration document.
func (s *Store) AthleteConfig(
	ctx context.Context, athleteID int64,
) (store.AthleteConfig, bool, error) {
	snapshot, err := s.collection(CollectionConfig).
		Doc(strconv.FormatInt(athleteID, 10)).Get(ctx)
	if err != nil {
		if notFound(err) {
			return store.AthleteConfig{}, false, nil
		}

		return store.AthleteConfig{}, false, fmt.Errorf("firestore: read athlete config: %w", err)
	}

	var doc configDoc
	if err := snapshot.DataTo(&doc); err != nil {
		return store.AthleteConfig{}, false, fmt.Errorf("firestore: decode athlete config: %w", err)
	}

	franchises := make([]store.Franchise, 0, len(doc.Franchises))
	for _, entry := range doc.Franchises {
		franchises = append(franchises, store.Franchise{
			Name:       entry.Name,
			SportTypes: entry.SportTypes,
			GearName:   entry.GearName,
			Titles:     entry.Titles,
			Reserved:   entry.Reserved,
		})
	}

	return store.AthleteConfig{Franchises: franchises}, true, nil
}

// SaveAthleteConfig replaces the document.
func (s *Store) SaveAthleteConfig(
	ctx context.Context, athleteID int64, config store.AthleteConfig,
) error {
	entries := make([]franchiseEntry, 0, len(config.Franchises))
	for _, franchise := range config.Franchises {
		entries = append(entries, franchiseEntry{
			Name:       franchise.Name,
			SportTypes: franchise.SportTypes,
			GearName:   franchise.GearName,
			Titles:     franchise.Titles,
			Reserved:   franchise.Reserved,
		})
	}

	doc := configDoc{
		AthleteID:  athleteID,
		Franchises: entries,
		UpdatedAt:  time.Now().UTC(),
	}

	_, err := s.collection(CollectionConfig).
		Doc(strconv.FormatInt(athleteID, 10)).Set(ctx, doc)
	if err != nil {
		return fmt.Errorf("firestore: write athlete config: %w", err)
	}

	return nil
}
