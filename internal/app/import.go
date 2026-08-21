package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/importer"
	"github.com/jkreileder/titelheld/internal/strava"
)

// Import seeds the title history from the athlete's Strava past.
//
// Wired here rather than in the command for the same reason Run is: the
// command is a shim, and everything with behavior stays testable.
//
// It resolves the athlete the way the OAuth callback binds one — the single
// stored token, refusing on none or several — rather than taking an ID from a
// flag. An athlete ID is a number nobody can check by eye, and the wrong one
// would write a stranger's history into this athlete's log.
//
// Safe to run only while nothing else is naming, which is the normal state:
// the service scales to zero and the scheduler is paused. It shares the token
// source with the service, so a refresh here rotates the stored refresh token
// exactly as one there would.
func Import(ctx context.Context, logger *slog.Logger, getenv func(string) string) error {
	cfg, err := config.LoadImport(getenv)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	if !cfg.PersistentStore() {
		return errors.New(
			"import: no Firestore configured; an import against the in-memory store " +
				"would write to a database that disappears when this process exits")
	}

	dataStore, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	defer closeStore()

	token, err := dataStore.AnyToken(ctx)
	if err != nil {
		return fmt.Errorf(
			"import: no single bound athlete; run the authorization flow first: %w", err)
	}

	client, err := importClient(cfg, dataStore, token.AthleteID)
	if err != nil {
		return err
	}

	return importWith(ctx, cfg, dataStore, client, token.AthleteID, logger)
}

// importWith runs the import against an already-open store.
//
// Split from [Import] so everything after the store is open can be tested: the
// athlete resolution, the read-only client, and the run itself. Import keeps
// only what needs a real environment — reading it, refusing an in-memory one,
// and opening Firestore.
//
// The listing and the athlete both arrive resolved, so this has no branch a
// test cannot reach and no second lookup that could disagree with the first:
// the client authenticates as whoever Import resolved, and the history is
// written under that same athlete.
func importWith(
	ctx context.Context, cfg config.Config, dataStore boundStore,
	activities importer.Activities, athleteID int64, logger *slog.Logger,
) error {
	rules, err := classifierConfig(cfg)
	if err != nil {
		return err
	}

	result, err := importer.Run(ctx, importer.Deps{
		Activities:    activities,
		Store:         dataStore,
		AthleteID:     athleteID,
		MachineTitles: rules.MachineTitles,
		Logger:        logger,
	})

	// Reported either way. A run that stopped halfway still wrote what it
	// wrote, and the next run continues from there.
	logger.Info("import finished",
		"seen", result.Seen,
		"imported", result.Imported,
		"skipped", result.Skipped,
		"already_known", result.AlreadyKnown,
		"pages", result.Pages)

	if err != nil {
		return err
	}

	return nil
}

// importClient builds the read-only Strava client an import uses.
//
// Left in the client's dry-run zero value on purpose: its transport refuses
// every request that is not a GET, so "an import cannot change an activity"
// holds however the code above it changes rather than by anyone remembering.
//
// One request an import makes does not go through it. The token source
// refreshes an expired access token by POSTing to Strava's /oauth/token, on
// the OAuth type's own client. That is not a gap in the guarantee — the
// endpoint issues tokens and cannot touch an activity — and it is not
// avoidable either: refusing to refresh would mean an import that fails
// whenever the stored token has aged past its few hours, which is most of the
// time. What matters is that nothing reachable from here can write an
// activity, and the client above is what enforces that.
func importClient(
	cfg config.Config, dataStore boundStore, athleteID int64,
) (*strava.Client, error) {
	oauth := &strava.OAuth{
		ClientID:     cfg.StravaClientID,
		ClientSecret: cfg.StravaClientSecret,
		RedirectURL:  cfg.RedirectURL(),
	}

	client, err := strava.NewClient(strava.ClientConfig{
		Tokens: strava.NewStoredTokenSource(oauth, dataStore, athleteID),
	})
	if err != nil {
		return nil, fmt.Errorf("import: build the Strava client: %w", err)
	}

	return client, nil
}
