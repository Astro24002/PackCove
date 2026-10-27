package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Astro24002/PackCove/internal/packcove"
)

func main() {
	configPath := flag.String("config", "packcove.yaml", "path to the PackCove YAML configuration")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := packcove.LoadConfig(*configPath)
	if err != nil {
		logger.Error("configuration invalid", "error", err)
		os.Exit(1)
	}
	service, err := packcove.NewService(cfg, logger)
	if err != nil {
		logger.Error("service initialization failed", "error", err)
		os.Exit(1)
	}
	defer service.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
