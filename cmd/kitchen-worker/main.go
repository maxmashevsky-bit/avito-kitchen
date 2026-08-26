package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/config"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/outbox"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/logging"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/postgres"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	pool, err := postgres.Open(ctx, cfg.KitchenDatabaseURL)
	if err != nil {
		stop()
		logger.Error("open kitchen database", "error", err)
		os.Exit(1)
	}
	worker := outbox.New(pool, logger, cfg.IntegrationAPIKey, cfg.WorkerPollInterval, cfg.WorkerHTTPTimeout, cfg.WorkerMaxAttempts)
	err = worker.Run(ctx)
	pool.Close()
	stop()
	if err != nil {
		logger.Error("kitchen worker stopped", "error", err)
		os.Exit(1)
	}
}
