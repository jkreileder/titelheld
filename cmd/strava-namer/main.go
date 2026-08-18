// Command strava-namer is the Cloud Run entry point. It wires the packages
// together and serves the HTTP surface; all the behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/jkreileder/strava-namer/internal/config"
	"github.com/jkreileder/strava-namer/internal/server"
	"github.com/jkreileder/strava-namer/internal/store"
	"github.com/jkreileder/strava-namer/internal/strava"
	"github.com/jkreileder/strava-namer/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := run(ctx, logger, os.Getenv)

	// Release the signal handler before exiting: os.Exit skips deferred calls.
	stop()

	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, getenv func(string) string) error {
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
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	logger.Info("configuration loaded",
		"process_delay", cfg.ProcessDelay,
		"athlete_id", cfg.AthleteID,
		"auth_path", config.AuthPath,
		"store", "memory")

	return httpServer.Run(ctx, net.JoinHostPort("", strconv.Itoa(cfg.Port)))
}
