package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/config"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/logging"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/postgres"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/platform/run"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/restaurantdemo"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-maxmashevsky-bit-816a859e/internal/restauranthttp"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.LogLevel)
	pool, err := postgres.Open(context.Background(), cfg.RestaurantDatabaseURL)
	if err != nil {
		logger.Error("open restaurant database", "error", err)
		os.Exit(1)
	}
	publisher := restaurantdemo.New(pool)
	ctx, cancel := context.WithCancel(context.Background())
	go publisher.PublishMenuUntilSuccess(ctx, logger, cfg.KitchenAPIURL, cfg.DemoPartnerAPIKey)
	handler := restauranthttp.New(pool, cfg.IntegrationAPIKey, cfg.MaxBodyBytes)
	server := &http.Server{Addr: cfg.RestaurantHTTPAddr, Handler: restauranthttp.Router(handler, logger), ReadHeaderTimeout: cfg.ReadTimeout, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 1 << 20}
	err = run.HTTP(ctx, logger, server, cfg.ShutdownTimeout)
	cancel()
	pool.Close()
	if err != nil {
		logger.Error("restaurant demo stopped", "error", err)
		os.Exit(1)
	}
}
