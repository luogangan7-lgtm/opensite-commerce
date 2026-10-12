// API executable is the composition entry point: env, signals and bootstrap.
// It supplies the database transaction capability to cart and depends on pgx,
// bootstrap and stdlib. Customer session resolution remains a separate seam.
package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
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
		pool, err := pgxpool.New(ctx, dsn)
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
