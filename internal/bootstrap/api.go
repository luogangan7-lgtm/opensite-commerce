// Package bootstrap statically wires the API using platform/httpapi and observability.
// cmd/api calls it; it owns no business handlers, database pool or migrations.
// Generated business HTTP adapters stay in platform/httpapi and consume public ports.
// Registration errors stop construction, so invalid routes cannot be served.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/carthttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/catalogcsvhttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/datarightshttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

type Config struct {
	Subject       httpapi.SubjectResolver
	Privacy       datarightshttp.Config
	CatalogCSV    catalogcsvhttp.Config
	Address       string
	Logger        *slog.Logger
	Authenticator httpapi.Authenticator
	Routes        []httpapi.Route
	Catalog       catalog.Config
	Media         media.Config
	Cart          *cart.Module
}

func New(config Config) (server *http.Server, err error) {
	defer func() {
		if p := recover(); p != nil {
			server = nil
			err = fmt.Errorf("API static composition failed: %v", p)
		}
	}()
	cartModule := config.Cart
	if cartModule == nil {
		cartModule = cart.New(nil, nil)
	}
	router := httpapi.New(routeAuthenticator{cart: cartModule, staff: config.Authenticator}, config.Logger)
	health := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "health", Method: "GET", Pattern: "/healthz", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	routes := []httpapi.Route{health}
	routes = append(routes, catalog.New(config.Catalog).Routes()...)
	routes = append(routes, media.New(config.Media).Routes()...)
	routes = append(routes, carthttp.NewCartHandler(cartModule).Routes()...)
	config.Privacy.Subject, config.CatalogCSV.Subject = config.Subject, config.Subject
	routes = append(routes, datarightshttp.New(config.Privacy).Routes()...)
	routes = append(routes, catalogcsvhttp.New(config.CatalogCSV).Routes()...)
	routes = append(routes, config.Routes...)
	for _, route := range routes {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			return nil, err
		}
	}
	metrics := &observability.Metrics{}
	router.WrapTransport(metrics.Wrap)
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

// routeAuthenticator separates storefront capabilities from staff authentication.
// A staff credential can never authorize a Storefront route by falling through.
type routeAuthenticator struct {
	cart  *cart.Module
	staff httpapi.Authenticator
}

func (a routeAuthenticator) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.Access == httpapi.Storefront {
		return a.cart.Authenticate(ctx, r, spec)
	}
	if a.staff == nil {
		return (httpapi.DenyAuthenticator{}).Authenticate(ctx, r, spec)
	}
	return a.staff.Authenticate(ctx, r, spec)
}
