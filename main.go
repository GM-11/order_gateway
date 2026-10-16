package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("bad config", "err", err)
		os.Exit(2)
	}

	// ctx is cancelled on Ctrl-C or SIGTERM (what Kubernetes sends before killing a pod).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}
