// Command titelheld-import seeds the title history from Strava.
//
// A one-shot, run by hand against the real Firestore with the operator's own
// credentials. It is a shim: the wiring lives in internal/app.
//
//	FIRESTORE_PROJECT=… FIRESTORE_DATABASE=… \
//	STRAVA_CLIENT_ID=… STRAVA_CLIENT_SECRET=… \
//	BASE_URL=… WEBHOOK_PATH_SECRET=… STRAVA_VERIFY_TOKEN=… \
//	go run ./cmd/titelheld-import
//
// It never writes to Strava: the client is built in its dry-run zero value and
// the transport refuses anything that is not a GET.
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

	err := app.Import(ctx, logger, os.Getenv)

	stop()

	if err != nil {
		logger.Error("import failed", "error", err)
		os.Exit(1)
	}
}
