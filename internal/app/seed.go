package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/logsafe"
	"github.com/jkreileder/titelheld/internal/naming"
	"github.com/jkreileder/titelheld/internal/processor"
	"github.com/jkreileder/titelheld/internal/store"
)

// SeedConfig writes the athlete's first configuration document.
//
// The shipped default profile applies only while no document exists, so the
// first curation change — reserving an entry, adding a series — is made by
// creating the document, seeded from that profile, and the document is
// authoritative from then on. This is the one command that creates it. It
// refuses to touch an existing document: after the first write the athlete
// edits it in place, and a re-run that replaced it would undo every edit made
// since.
//
// A one-shot run by hand, like the import and for the same reasons: the
// service is invokable by allUsers, and a job run once under the operator's
// own credentials has no business being an endpoint. It talks to nothing but
// Firestore — no Strava client is built and no token is refreshed — so the
// only configuration it reads is where the database is.
//
// The athlete is resolved the way the import resolves one: from the single
// stored token, refusing on none or several. An athlete ID is a number nobody
// checks by eye, and a document under the wrong one configures nobody while
// looking like it configured somebody.
//
// reserve names entries to mark reserved on top of what the profile already
// reserves; each must name exactly one entry in exactly one series.
func SeedConfig(
	ctx context.Context, logger *slog.Logger, getenv func(string) string, reserve []string,
) error {
	cfg, err := config.LoadStore(getenv)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	dataStore, closeStore, athleteID, err := openBoundStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	defer closeStore()

	return seedConfigWith(ctx, dataStore, athleteID, reserve, logger)
}

// seedConfigWith writes the document against an already-open store.
//
// Split from [SeedConfig] so everything after the store is open can be tested:
// the refusal to overwrite, the reservation, the write, and the read-back.
func seedConfigWith(
	ctx context.Context, dataStore store.Store, athleteID int64,
	reserve []string, logger *slog.Logger,
) error {
	if _, exists, err := dataStore.AthleteConfig(ctx, athleteID); err != nil {
		return fmt.Errorf("seed: read the athlete configuration: %w", err)
	} else if exists {
		return fmt.Errorf(
			"seed: athlete %d already has a configuration document; it is authoritative — "+
				"edit it in place rather than seeding it again", athleteID)
	}

	profile, err := reserveEntries(naming.DefaultProfile(), reserve)
	if err != nil {
		return err
	}

	if err := dataStore.SaveAthleteConfig(ctx, athleteID, store.AthleteConfig{
		Franchises: processor.FranchisesToStored(profile),
	}); err != nil {
		return fmt.Errorf("seed: write the athlete configuration: %w", err)
	}

	// Read back through the conversion the sweep uses, and report what each
	// series would offer next, from the stored position. This is the check
	// that the document says what was meant: the log line a sweep prints
	// after a cold start says the same thing, hours later.
	written, exists, err := dataStore.AthleteConfig(ctx, athleteID)
	if err != nil || !exists {
		return fmt.Errorf("seed: the document did not read back: exists=%v, %w", exists, err)
	}

	franchises := processor.FranchisesFromStored(written.Franchises)

	// The service reads the document once per process. A warm instance keeps
	// the profile it already read — the shipped default, which still offers
	// what was just reserved — until it is replaced, so the line says so.
	logger.Info("wrote the athlete configuration; the service reads it at its next cold start",
		"athlete_id", athleteID,
		"franchises", len(franchises))

	for _, franchise := range franchises {
		position, err := dataStore.FranchisePosition(ctx, athleteID, franchise.Name)
		if err != nil {
			return fmt.Errorf("seed: read the position of %q: %w", franchise.Name, err)
		}

		next, index, ok := franchise.Next(position)

		logger.Info("franchise as stored",
			"franchise", logsafe.String(franchise.Name),
			"titles", len(franchise.Titles),
			"reserved", len(franchise.Reserved),
			"position", position,
			"offers", ok,
			"next", logsafe.String(next),
			"next_index", index)
	}

	return nil
}

// reserveEntries marks the named entries reserved.
//
// Each name must match exactly one title in exactly one series, by
// [naming.SameEntry] — the comparison the rotation itself applies, so a
// reservation written here is one [naming.Franchise.Next] will honor. A name that
// matches nothing is refused rather than written as an inert reservation,
// because an inert reservation looks in the document exactly like a working
// one and the difference is a film handed out by mistake.
func reserveEntries(profile []naming.Franchise, reserve []string) ([]naming.Franchise, error) {
	for _, name := range reserve {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("seed: an empty entry cannot be reserved")
		}

		matches := 0

		for index := range profile {
			if !slices.ContainsFunc(profile[index].Titles, func(title string) bool {
				return naming.SameEntry(title, name)
			}) {
				continue
			}

			matches++

			if !profile[index].IsReserved(name) {
				profile[index].Reserved = append(profile[index].Reserved, strings.TrimSpace(name))
			}
		}

		if matches != 1 {
			return nil, fmt.Errorf(
				"seed: %q names %d entries in the default profile, want exactly one",
				strings.TrimSpace(name), matches)
		}
	}

	return profile, nil
}
