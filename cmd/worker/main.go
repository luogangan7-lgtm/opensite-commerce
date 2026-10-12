//go:build pgx

// Worker composition uses a dedicated queue-processing login and a signing key
// supplied through the environment. Trusted configured tenants drive periodic
// expiry; queue JSON is verified before controlled tenant services are invoked.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	url := os.Getenv("WORKER_DATABASE_URL")
	if url == "" {
		return errors.New("WORKER_DATABASE_URL required")
	}
	key, err := hex.DecodeString(os.Getenv("JOB_SIGNING_KEY"))
	if err != nil {
		return errors.New("JOB_SIGNING_KEY must be hexadecimal")
	}
	signer, err := jobs.NewSigner(key)
	if err != nil {
		return err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return errors.New("invalid worker database configuration")
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE app_queue_worker")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("worker database pool unavailable")
	}
	defer pool.Close()
	var tenants []string
	if value := strings.TrimSpace(os.Getenv("JOB_TENANTS")); value != "" {
		for _, tenant := range strings.Split(value, ",") {
			tenants = append(tenants, strings.TrimSpace(tenant))
		}
	}
	worker, err := jobs.NewWorker(pool, signer, jobs.WorkerConfig{Tenants: tenants})
	if err != nil {
		return err
	}
	if err := worker.Start(ctx); err != nil {
		return errors.New("worker startup failed")
	}
	<-ctx.Done()
	stop, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	return worker.Stop(stop)
}
