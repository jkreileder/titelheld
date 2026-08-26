// Command titelheld-config writes the athlete's first configuration document.
//
// A one-shot, run by hand against the real Firestore with the operator's own
// credentials, and only once: the document it creates is authoritative from
// then on and is edited in place. It is a shim: the wiring lives in
// internal/app.
//
//	FIRESTORE_PROJECT=… FIRESTORE_DATABASE=… \
//	go run ./cmd/titelheld-config -reserve "Son of the Pink Panther"
//
// It seeds the document from the shipped default profile, reserving on top of
// it every entry named with -reserve, and refuses if a document already
// exists. It talks to nothing but Firestore.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jkreileder/titelheld/internal/app"
)

// repeatable collects a flag given more than once.
type repeatable []string

func (r *repeatable) String() string { return "" }

func (r *repeatable) Set(value string) error {
	*r = append(*r, value)

	return nil
}

func main() {
	var reserve repeatable

	flag.Var(&reserve, "reserve",
		"an entry of the default profile to reserve for the athlete to spend by hand; repeatable")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := app.SeedConfig(ctx, logger, os.Getenv, reserve)

	stop()

	if err != nil {
		logger.Error("seeding the configuration failed", "error", err)
		os.Exit(1)
	}
}
