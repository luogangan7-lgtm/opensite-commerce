// API executable is the composition entry point: env, signals and bootstrap.
// Dependencies: stdlib/bootstrap/quotes application; module handlers (including
// staff login) are installed through bootstrap, HTTP lives in the platform.
// Quotes deny by default until trusted staff/customer/host authentication is wired.
// Invalid registration fails startup; this executable delegates business IO to modules.
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
