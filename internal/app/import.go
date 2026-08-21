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
	cfg, err := config.Load(getenv)
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

	oauth := &strava.OAuth{
		ClientID:     cfg.StravaClientID,
		ClientSecret: cfg.StravaClientSecret,
		RedirectURL:  cfg.RedirectURL(),
	}

	// Read-only work, so the client is left in its safe zero value. An import
	// has no business being able to write, and this is the cheapest way to
	// guarantee it: the transport refuses every non-GET request.
	client, err := strava.NewClient(strava.ClientConfig{
		Tokens: strava.NewStoredTokenSource(oauth, dataStore, token.AthleteID),
	})
	if err != nil {
		return fmt.Errorf("import: build the Strava client: %w", err)
	}

	rules, err := classifierConfig(cfg)
	if err != nil {
		return err
	}

	result, err := importer.Run(ctx, importer.Deps{
		Activities:    client,
		Store:         dataStore,
		AthleteID:     token.AthleteID,
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
