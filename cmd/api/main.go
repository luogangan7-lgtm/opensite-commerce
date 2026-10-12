// API executable is the composition entry point: env, signals and bootstrap.
// It supplies the database transaction capability to cart through pgxadapter,
// bootstrap and stdlib. Customer session resolution remains a separate seam.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	address := os.Getenv("OSC_API_ADDR")
	if address == "" {
		address = ":8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config := bootstrap.Config{Address: address, Logger: logger}
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		pool, err := pgxadapter.NewPool(ctx, dsn, "")
		if err != nil {
			logger.Error("Invalid database configuration")
			os.Exit(1)
		}
		defer pool.Close()
		config.Cart = cart.NewPostgres(pool, nil)
	}
	if err := bootstrap.Run(ctx, config); err != nil {
		logger.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
