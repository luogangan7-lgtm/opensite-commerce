//go:build pgx

// Responsibility: worker operational listener, credentials and aggregate query.
// Dependencies: observability, approved pgxadapter pool and stdlib; main composes it.
// Invariants: fixed login roles, no admin pool, bounded HTTP and secret-safe failures.
package main

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

type metricQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type outboxMetrics struct{ query metricQuerier }

func (s outboxMetrics) Snapshot(ctx context.Context) (observability.WorkerSnapshot, error) {
	if s.query == nil {
		return observability.WorkerSnapshot{}, errors.New("worker metrics unavailable")
	}
	var pending, dead int64
	var age float64
	err := s.query.QueryRow(ctx, "SELECT pending, dead_letters, oldest_pending_age_seconds FROM app.outbox_metrics()").Scan(&pending, &dead, &age)
	if err != nil || pending < 0 || dead < 0 || age < 0 || math.IsNaN(age) || math.IsInf(age, 0) || age >= float64(math.MaxInt64)/float64(time.Second) {
		return observability.WorkerSnapshot{}, errors.New("worker metrics unavailable")
	}
	return observability.WorkerSnapshot{Pending: uint64(pending), DeadLetters: uint64(dead), OldestPendingAge: time.Duration(age * float64(time.Second))}, nil
}

type observedWorker struct {
	jobWorker
	source observability.WorkerMetrics
}

func (w *observedWorker) MetricsSource() observability.WorkerMetrics { return w.source }

// workerDSN accepts legacy explicit DSNs or local compose component credentials.
// UserPassword encodes punctuation without injecting extra connection options.
func workerDSN(getenv func(string) string, variable, kind, login string) (string, error) {
	if explicit := getenv(variable); explicit != "" {
		return explicit, nil
	}
	password := getenv("OPENSITE_" + kind + "_DB_PASSWORD")
	if password == "" {
		return "", nil
	}
	host, database := getenv("OPENSITE_DB_HOST"), getenv("OPENSITE_DB")
	if database == "" {
		database = "opensite"
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`).MatchString(host) || !regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`).MatchString(database) || len(password) < 24 {
		return "", errors.New("database component configuration is invalid")
	}
	dsn := url.URL{Scheme: "postgresql", User: url.UserPassword(login, password), Host: host + ":5432", Path: "/" + database, RawQuery: "sslmode=disable"}
	return dsn.String(), nil
}

type workerMonitor struct {
	server *http.Server
	ready  atomic.Bool
	done   chan error
	addr   string
}

func startWorkerMonitor(address string, source observability.WorkerMetrics) (*workerMonitor, error) {
	if address == "" {
		address = ":8081"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, errors.New("worker monitor listener unavailable")
	}
	monitor := &workerMonitor{done: make(chan error, 1), addr: listener.Addr().String()}
	monitor.ready.Store(true)
	monitor.server = &http.Server{Handler: observability.NewWorkerHandler(monitor.ready.Load, source), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	go func() {
		err := monitor.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		monitor.done <- err
	}()
	return monitor, nil
}
func (m *workerMonitor) Stop(ctx context.Context) error {
	m.ready.Store(false)
	if err := m.server.Shutdown(ctx); err != nil {
		_ = m.server.Close()
		return errors.New("worker monitor stop failed")
	}
	return nil
}
