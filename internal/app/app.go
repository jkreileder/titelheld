// Package app builds and runs the service.
//
// It lives here rather than in cmd/ so that everything with behaviour is
// testable: the command is a shim with nothing in it worth a test.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/jkreileder/strava-namer/internal/config"
	"github.com/jkreileder/strava-namer/internal/server"
	"github.com/jkreileder/strava-namer/internal/store"
	"github.com/jkreileder/strava-namer/internal/strava"
	"github.com/jkreileder/strava-namer/internal/webhook"
)

// Run wires the packages together and serves until ctx is cancelled.
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

	memory := store.NewMemory()

	oauth := &strava.OAuth{
		ClientID:     cfg.StravaClientID,
		ClientSecret: cfg.StravaClientSecret,
		RedirectURL:  cfg.RedirectURL(),
	}

	hook, err := webhook.New(webhook.Config{
		VerifyToken: cfg.StravaVerifyToken,
		AthleteID:   cfg.AthleteID,
		Delay:       cfg.ProcessDelay,
		Queue:       memory,
		Named:       memory,
		Logger:      logger,
	})
	if err != nil {
		return fmt.Errorf("build webhook: %w", err)
	}

	httpServer, err := server.New(server.Deps{
		Config:  cfg,
		OAuth:   oauth,
		Tokens:  memory,
		Webhook: hook,
		Logger:  logger,
		Bound: func(ctx context.Context) (int64, bool) {
			token, err := memory.AnyToken(ctx)

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
		"store", "memory")

	return httpServer.Run(ctx, net.JoinHostPort("", strconv.Itoa(cfg.Port)))
}
