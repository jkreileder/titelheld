// Command titelheld is the Cloud Run entry point. It is a shim: the wiring
// and everything else lives in internal/app.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jkreileder/titelheld/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := app.Run(ctx, logger, os.Getenv)

	// Release the signal handler before exiting: os.Exit skips deferred calls.
	stop()

	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}
