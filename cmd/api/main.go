// API executable is the composition entry point: env, signals and bootstrap.
// It depends only on stdlib/bootstrap; identity handlers are installed statically.
// Invalid registration fails startup; this executable delegates business IO to modules.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	config := bootstrap.Config{Address: address, Logger: logger}
	staff, err := bootstrap.StaffLogin(ctx, os.Getenv("OSC_AUTH_DATABASE_URL"), os.Getenv("OSC_ADMIN_ORIGIN"), os.Getenv("OSC_AUTH_SIGNING_KEY_HEX"), os.Getenv("OSC_AUTH_TRUSTED_PROXIES"))
	if err != nil {
		logger.Error("staff login startup failed")
		os.Exit(1)
	}
	if staff != nil {
		config.Authenticator = staff.Authenticator()
		config.Routes = append(config.Routes, staff.Routes()...)
		defer func() {
			closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = staff.Close(closing)
		}()
	}
	if err := bootstrap.Run(ctx, config); err != nil {
		logger.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
