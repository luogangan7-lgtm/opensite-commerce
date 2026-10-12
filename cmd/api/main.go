// API executable is the composition entry point: env, signals and bootstrap.
// It depends only on stdlib/bootstrap; all HTTP behavior lives in the platform.
// Invalid registration fails startup; this executable performs no business IO.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	address := os.Getenv("OSC_API_ADDR")
	if address == "" {
		address = ":8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	routes, closePublic, err := storefrontRoutes(ctx)
	if err != nil {
		logger.Error("Public read initialization failed", "error", err)
		os.Exit(1)
	}
	defer closePublic()
	config, closeRuntime, err := bootstrap.FromEnvironment(ctx, bootstrap.Config{Address: address, Logger: logger, Routes: routes})
	if err != nil {
		logger.Error("API configuration failed", "error", err)
		os.Exit(1)
	}
	defer closeRuntime()
	if err := bootstrap.Run(ctx, config); err != nil {
		logger.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
