package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/config"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/kitchenhttp"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/logging"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/postgres"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/run"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel)
	pool, err := postgres.Open(context.Background(), cfg.KitchenDatabaseURL)
	if err != nil {
		logger.Error("open kitchen database", "error", err)
		os.Exit(1)
	}
	handler := kitchenhttp.New(pool, cfg.MaxBodyBytes)
	server := &http.Server{Addr: cfg.KitchenHTTPAddr, Handler: kitchenhttp.Router(handler, logger), ReadHeaderTimeout: cfg.ReadTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 1 << 20}
	err = run.HTTP(context.Background(), logger, server, cfg.ShutdownTimeout)
	pool.Close()
	if err != nil {
		logger.Error("kitchen API stopped", "error", err)
		os.Exit(1)
	}
}
