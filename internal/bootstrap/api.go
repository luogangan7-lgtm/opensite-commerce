// Package bootstrap statically wires the API using platform/httpapi only.
// cmd/api calls it; it owns no business handlers, database pool or migrations.
// Registration errors stop construction, so invalid routes cannot be served.
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Config struct {
	Address       string
	Logger        *slog.Logger
	Authenticator httpapi.Authenticator
	Routes        []httpapi.Route
}

func New(config Config) (*http.Server, error) {
	router := httpapi.New(config.Authenticator, config.Logger)
	health := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "health", Method: "GET", Pattern: "/healthz", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	for _, route := range append([]httpapi.Route{health}, config.Routes...) {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			return nil, err
		}
	}
	// Socket write deadlines do not cancel PostgreSQL queries or lock waits.
	// This covers authentication and handlers together, including the 60s backoff.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
		defer cancel()
		router.ServeHTTP(w, r.WithContext(ctx))
	})
	return &http.Server{Addr: config.Address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}, nil
}

func Run(ctx context.Context, config Config) error {
	server, err := New(config)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			<-done
			return err
		}
		<-done
		return nil
	}
}
