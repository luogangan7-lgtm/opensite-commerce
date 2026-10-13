// Package bootstrap statically wires catalog/media/cart and optional checkout HTTP.
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
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type Config struct {
	Address       string
	Logger        *slog.Logger
	Authenticator httpapi.Authenticator
	Routes        []httpapi.Route
	Catalog       catalog.Config
	Media         media.Config
	Cart          *cart.Module
	Checkout      *checkout.Config
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
	var authenticator httpapi.Authenticator = routeAuthenticator{cart: cartModule, staff: config.Authenticator}
	var checkoutRoutes []httpapi.Route
	if config.Checkout != nil {
		handler, err := checkout.New(*config.Checkout)
		if err != nil {
			return nil, err
		}
		authenticator = handler.Authenticator(authenticator)
		checkoutRoutes = handler.Routes()
	}
	router := httpapi.New(authenticator, config.Logger)
	health := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "health", Method: "GET", Pattern: "/healthz", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	routes := []httpapi.Route{health}
	routes = append(routes, catalog.New(config.Catalog).Routes()...)
	routes = append(routes, media.New(config.Media).Routes()...)
	routes = append(routes, carthttp.NewCartHandler(cartModule).Routes()...)
	routes = append(routes, checkoutRoutes...)
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
