// Package bootstrap statically wires the API using platform/httpapi only.
// cmd/api calls it; it owns no business handlers, database pool or migrations.
// Registration errors stop construction, so invalid routes cannot be served.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Config struct {
	Address                  string
	Logger                   *slog.Logger
	Authenticator            httpapi.Authenticator
	Routes                   []httpapi.Route
	Catalog                  catalog.Config
	Media                    media.Config
	Quotes                   httpapi.QuotesService
	QuotesBuyerAuthenticator httpapi.Authenticator
	QuotesInquiryResolver    httpapi.QuotesInquiryResolver
}

func New(config Config) (server *http.Server, err error) {
	defer func() {
		if p := recover(); p != nil {
			server = nil
			err = fmt.Errorf("API static composition failed: %v", p)
		}
	}()
	router := httpapi.New(config.Authenticator, config.Logger)
	health := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "health", Method: "GET", Pattern: "/healthz", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	routes := []httpapi.Route{health}
	routes = append(routes, catalog.New(config.Catalog).Routes()...)
	routes = append(routes, media.New(config.Media).Routes()...)
	if config.Quotes != nil {
		routes = append(routes, httpapi.NewQuotesHandler(config.Quotes, config.QuotesBuyerAuthenticator, config.QuotesInquiryResolver).Routes()...)
	}
	routes = append(routes, config.Routes...)
	for _, route := range routes {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			return nil, err
		}
	}
	return &http.Server{Addr: config.Address, Handler: router, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}, nil
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
