// API executable is the composition entry point: env, signals and bootstrap.
// Dependencies: stdlib/bootstrap/quotes application; HTTP lives in the platform.
// The quote runner and customer/host resolvers are intentionally unconfigured;
// every quote/contact endpoint denies authentication until trusted wiring exists.
// Invalid registration fails startup; this executable performs no business IO.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	address := os.Getenv("OSC_API_ADDR")
	if address == "" {
		address = ":8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := bootstrap.Run(ctx, bootstrap.Config{Address: address, Logger: logger, Quotes: quotes.NewApplication(nil)}); err != nil {
		logger.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
