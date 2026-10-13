// Package bootstrap wires catalog, media, cart, injected routes and operational telemetry.
// Dependencies: module constructors, platform/httpapi and observability; caller: cmd/api.
// It owns no business tables or pools. Registration errors refuse startup; request
// contexts are bounded, route snapshots immutable and metric labels nonsensitive.
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
	notificationcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/carthttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/notificationhttp"
	quoteshttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quotes"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

type Config struct {
	Address                  string
	Logger                   *slog.Logger
	Authenticator            httpapi.Authenticator
	Routes                   []httpapi.Route
	Catalog                  catalog.Config
	Media                    media.Config
	Cart                     *cart.Module
	Quotes                   quoteshttp.QuotesService
	QuotesBuyerAuthenticator httpapi.Authenticator
	QuotesInquiryResolver    quoteshttp.QuotesInquiryResolver
	CMS                      publish.CMSHTTPConfig
	Notifications            notificationcontract.Service
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
	routes = append(routes, publish.NewCMSHandler(config.CMS).Routes()...)
	if config.Quotes != nil {
		routes = append(routes, quoteshttp.NewQuotesHandler(config.Quotes, config.QuotesBuyerAuthenticator, config.QuotesInquiryResolver).Routes()...)
	}
	routes = append(routes, notificationhttp.New(config.Notifications).Routes()...)
	routes = append(routes, config.Routes...)
	for _, route := range routes {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			return nil, err
		}
	}
	metrics := &observability.Metrics{}
	router.WrapTransport(metrics.Wrap)
	// Socket deadlines do not cancel database queries or lock waits. Preserve
	// the router's registration snapshot through the request deadline wrapper.
	handler := deadlineHandler{Router: router}
	return &http.Server{Addr: config.Address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}, nil
}

// Embedding exposes the router's immutable RouteSpecs snapshot for contract gates.
type deadlineHandler struct{ *httpapi.Router }

func (h deadlineHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	h.Router.ServeHTTP(w, r.WithContext(ctx))
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

// ResolveActor preserves the identity projection of the selected staff
// authenticator. It never manufactures an actor from an HTTP field.
func (a routeAuthenticator) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	if spec.Access == httpapi.Storefront {
		return httpapi.Actor{}, nil
	}
	if resolver, ok := a.staff.(httpapi.ActorResolver); ok {
		return resolver.ResolveActor(ctx, r, spec)
	}
	return httpapi.Actor{}, nil
}
