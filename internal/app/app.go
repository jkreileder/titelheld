// Package app builds and runs the service.
//
// It lives here rather than in cmd/ so that everything with behavior is
// testable: the command is a shim with nothing in it worth a test.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/jkreileder/titelheld/internal/config"
	"github.com/jkreileder/titelheld/internal/server"
	"github.com/jkreileder/titelheld/internal/store"
	firestorestore "github.com/jkreileder/titelheld/internal/store/firestore"
	"github.com/jkreileder/titelheld/internal/strava"
	"github.com/jkreileder/titelheld/internal/webhook"
)

// Run wires the packages together and serves until ctx is canceled.
//
// getenv is a parameter rather than a call to os.Getenv so the whole service
// can be started in a test without touching the process environment.
func Run(ctx context.Context, logger *slog.Logger, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	// Say plainly, on every start, whether this process can touch the athlete's
	// activities.
	if cfg.DryRun() {
		logger.Info("dry run: Strava writes are disabled")
	} else {
		logger.Warn("WRITES ENABLED: this process will rename real Strava activities")
	}

	dataStore, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		return err
	}

	defer closeStore()

	oauth := &strava.OAuth{
		ClientID:     cfg.StravaClientID,
		ClientSecret: cfg.StravaClientSecret,
		RedirectURL:  cfg.RedirectURL(),
	}

	hook, err := webhook.New(webhook.Config{
		VerifyToken: cfg.StravaVerifyToken,
		AthleteID:   cfg.AthleteID,
		Delay:       cfg.ProcessDelay,
		Queue:       dataStore,
		Named:       dataStore,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("build webhook: %w", err)
	}

	httpServer, err := server.New(server.Deps{
		Config:  cfg,
		OAuth:   oauth,
		Tokens:  dataStore,
		Webhook: hook,
		Logger:  logger,
		Bound: func(ctx context.Context) (int64, bool) {
			token, err := dataStore.AnyToken(ctx)

			return token.AthleteID, err == nil
		},
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	logger.Info("configuration loaded",
		"process_delay", cfg.ProcessDelay,
		"athlete_id", cfg.AthleteID,
		"auth_path", cfg.AuthPath,
		"store", storeKind(cfg))

	return httpServer.Run(ctx, net.JoinHostPort("", strconv.Itoa(cfg.Port)))
}

// boundStore is the store plus the bootstrap lookup the server needs.
type boundStore interface {
	store.Store

	AnyToken(ctx context.Context) (strava.Token, error)
}

func storeKind(cfg config.Config) string {
	if cfg.PersistentStore() {
		return "firestore"
	}

	return "memory"
}

// openStore picks the persistent store when one is configured.
//
// The in-memory fallback exists for local runs. It is called out loudly because
// it silently loses the OAuth token pair on restart, and Strava has already
// invalidated the refresh token that would let the service recover.
func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (boundStore, func(), error) {
	if !cfg.PersistentStore() {
		logger.Warn("using the in-memory store: state is lost on restart, including the OAuth token")

		return store.NewMemory(), func() {}, nil
	}

	firestoreStore, err := firestorestore.New(ctx, firestorestore.Config{
		ProjectID: cfg.FirestoreProject,
		Database:  cfg.FirestoreDatabase,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open firestore store: %w", err)
	}

	logger.Info("using the Firestore store",
		"project", cfg.FirestoreProject, "database", cfg.FirestoreDatabase)

	return firestoreStore, func() {
		if err := firestoreStore.Close(); err != nil {
			logger.Error("closing the Firestore client", "error", err)
		}
	}, nil
}
