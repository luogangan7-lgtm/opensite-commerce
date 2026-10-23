// Package bootstrap wires catalog, media, cart, customer, orders, CMS, fulfillment/returns, optional checkout, injected routes and operational telemetry.
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

	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart"
	cartcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/catalog"
	customercontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/media"
	notificationcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/pricing"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/carthttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/catalogcsvhttp"
	checkout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/datarightshttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/notificationhttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quoteorders"
	quoteshttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quotes"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

type Config struct {
	Payment                  payment.Port
	Stripe                   *payments.Stripe
	Subject                  httpapi.SubjectResolver
	Privacy                  datarightshttp.Config
	CatalogCSV               catalogcsvhttp.Config
	Address                  string
	Logger                   *slog.Logger
	Authenticator            httpapi.Authenticator
	Routes                   []httpapi.Route
	Catalog                  catalog.Config
	Pricing                  pricing.Config
	Media                    media.Config
	Orders                   orders.HTTPConfig
	Fulfillment              fulfillment.HTTPConfig
	Returns                  returns.HTTPConfig
	Cart                     *cart.Module
	Quotes                   quoteshttp.QuotesService
	QuoteOrders              quoteorders.Service
	QuoteConversion          *QuoteConversionConfig
	QuotesBuyerAuthenticator httpapi.Authenticator
	QuotesInquiryResolver    quoteshttp.QuotesInquiryResolver
	CMS                      publish.CMSHTTPConfig
	Notifications            notificationcontract.Service
	Checkout                 *checkout.Config
	Customer                 customercontract.Service
	CustomerOrigin           string
	CustomerSecret           []byte
	// ReturnsCoordinator is an unconsumed trusted composition seam; it enables no HTTP operation.
	ReturnsCoordinator returns.Config
}

func New(config Config) (server *http.Server, err error) {
	defer func() {
		if p := recover(); p != nil {
			server = nil
			err = fmt.Errorf("API static composition failed: %v", p)
		}
	}()
	if config.Subject == nil {
		config.Subject = staffSubject
	}
	cartModule := config.Cart
	if cartModule == nil {
		cartModule = cart.New(nil, nil)
	}
	customerHTTP := customerhttp.New(customerhttp.Config{Service: config.Customer, Origin: config.CustomerOrigin, Secret: config.CustomerSecret})
	if config.QuoteConversion != nil {
		if config.QuoteOrders != nil || config.Checkout == nil || config.Customer == nil {
			return nil, errors.New("typed quote conversion requires customer and checkout capabilities and excludes QuoteOrders injection")
		}
		config.QuoteOrders, err = NewQuoteConversion(*config.QuoteConversion, config.Checkout.Service)
		if err != nil {
			return nil, err
		}
		if config.QuotesBuyerAuthenticator == nil {
			config.QuotesBuyerAuthenticator = quoteBuyerAdmission{customer: customerHTTP, runner: config.QuoteConversion.Runner, inquiry: config.QuotesInquiryResolver}
		}
	}
	if config.QuotesBuyerAuthenticator != nil && config.QuoteOrders == nil {
		return nil, errors.New("quote buyer admission requires a configured conversion service")
	}
	if config.Customer != nil {
		cartModule = cartModule.WithActorResolver(func(ctx context.Context, _ *http.Request) (cartcontract.AuthorizedActor, error) {
			actor, ok := customercontract.ActorFromContext(ctx)
			if !ok {
				return cartcontract.AuthorizedActor{}, nil
			}
			return cartcontract.AuthorizedActor{TenantID: actor.TenantID, CustomerID: actor.CustomerID}, nil
		})
	}
	baseAuthenticator := routeAuthenticator{cart: cartModule, staff: config.Authenticator, customer: customerHTTP, buyer: config.QuotesBuyerAuthenticator}
	var authenticator httpapi.Authenticator = baseAuthenticator
	var checkoutRoutes []httpapi.Route
	if config.Checkout != nil {
		checkoutConfig := *config.Checkout
		var admissionCustomer *customerhttp.Handler
		guestResolver, hasGuest := checkoutConfig.Resolver.(*checkout.GuestResolver)
		if hasGuest && guestResolver == nil {
			return nil, errors.New("checkout guest resolver unavailable")
		}
		if hasGuest {
			checkoutConfig.Resolver = customerGuestCheckoutResolver{guest: guestResolver}
		}
		if config.Customer != nil && (checkoutConfig.Resolver == nil || hasGuest) {
			if checkoutConfig.Origin == "" {
				checkoutConfig.Origin = config.CustomerOrigin
			}
			if checkoutConfig.Origin != config.CustomerOrigin {
				return nil, errors.New("customer checkout requires the configured customer origin")
			}
			if !hasGuest {
				checkoutConfig.Resolver = customerCheckoutResolver{}
			}
			admissionCustomer = customerHTTP
		}
		handler, err := checkout.New(checkoutConfig)
		if err != nil {
			return nil, err
		}
		authenticator = checkoutAdmission{next: handler.Authenticator(baseAuthenticator), actors: baseAuthenticator, customer: admissionCustomer, cart: cartModule, guest: hasGuest}
		checkoutRoutes = handler.Routes()
	}
	router := httpapi.New(authenticator, config.Logger)
	health := httpapi.Route{Spec: httpapi.RouteSpec{OperationID: "health", Method: "GET", Pattern: "/healthz", Access: httpapi.Public}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })}
	routes := []httpapi.Route{health}
	stripeRoute := payments.StripeRoute(nil)
	if config.Stripe != nil {
		stripeRoute = payments.StripeRoute(config.Stripe)
	}
	routes = append(routes, stripeRoute)
	routes = append(routes, catalog.New(config.Catalog).Routes()...)
	routes = append(routes, pricing.New(config.Pricing).Routes()...)
	routes = append(routes, media.New(config.Media).Routes()...)
	routes = append(routes, orders.NewHTTP(config.Orders).Routes()...)
	routes = append(routes, fulfillment.NewHTTP(config.Fulfillment).Routes()...)
	routes = append(routes, returns.NewHTTP(config.Returns).Routes()...)
	routes = append(routes, carthttp.NewCartHandler(cartModule).Routes()...)
	routes = append(routes, publish.NewCMSHandler(config.CMS).Routes()...)
	routes = append(routes, checkoutRoutes...)
	if config.Quotes != nil {
		routes = append(routes, quoteshttp.NewQuotesHandler(config.Quotes, config.QuotesBuyerAuthenticator, config.QuotesInquiryResolver).Routes()...)
	}
	routes = append(routes, quoteorders.New(config.QuoteOrders).Routes()...)
	routes = append(routes, notificationhttp.New(config.Notifications).Routes()...)
	config.Privacy.Subject, config.CatalogCSV.Subject = config.Subject, config.Subject
	routes = append(routes, datarightshttp.New(config.Privacy).Routes()...)
	routes = append(routes, catalogcsvhttp.New(config.CatalogCSV).Routes()...)
	routes = append(routes, customerHTTP.Routes()...)
	routes = append(routes, config.Routes...)
	for _, route := range routes {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			return nil, err
		}
	}
	metrics := &observability.Metrics{}
	router.WrapTransport(metrics.Wrap)
	router.WrapTransport(customerHTTP.WrapTransport)
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
	cart     *cart.Module
	staff    httpapi.Authenticator
	customer *customerhttp.Handler
	buyer    httpapi.Authenticator
}

func (a routeAuthenticator) AuthenticationResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, spec httpapi.RouteSpec) {
	if spec.Access != httpapi.Storefront && spec.Access != httpapi.BuyerCompany && spec.Access != httpapi.CustomerSession {
		if responder, ok := a.staff.(httpapi.AuthenticationResponder); ok {
			responder.AuthenticationResponse(ctx, w, r, spec)
		}
	}
}

func (a routeAuthenticator) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.Access == httpapi.CustomerSession {
		actor, err := a.customer.Resolve(ctx, r)
		if err != nil {
			return nil, err
		}
		if actor.CustomerID == "" || actor.TenantID != r.PathValue("tenant_id") {
			return nil, checkoutCustomerRequired()
		}
		return customercontract.WithActor(ctx, actor), nil
	}
	if spec.Access == httpapi.BuyerCompany {
		if a.buyer == nil {
			return (httpapi.DenyAuthenticator{}).Authenticate(ctx, r, spec)
		}
		return a.buyer.Authenticate(ctx, r, spec)
	}
	if spec.Access == httpapi.Storefront {
		actor, err := a.customer.Resolve(ctx, r)
		if err != nil {
			return ctx, err
		}
		if actor.CustomerID != "" {
			ctx = customercontract.WithActor(ctx, actor)
		}
		return a.cart.Authenticate(ctx, r, spec)
	}
	if a.staff == nil {
		return (httpapi.DenyAuthenticator{}).Authenticate(ctx, r, spec)
	}
	ctx, err := a.staff.Authenticate(ctx, r, spec)
	if err != nil || ctx == nil {
		return ctx, err
	}
	// The API key's issuer is the staff subject, never the key ID. Only the
	// identity module's admitted principal can supply this projection; tenant,
	// credential and scope checks have already succeeded on the selected route.
	if spec.Scope == "quotes:read" || spec.Scope == "quotes:write" {
		if actor, ok := identity.AuthorizationFromContext(ctx); ok && actor.TenantID == r.PathValue("tenant_id") {
			ctx = qc.WithTrustedActor(ctx, qc.ActorIdentity{TenantID: actor.TenantID, StaffID: actor.UserID})
		}
	}
	return ctx, nil
}

// ResolveActor preserves the identity projection of the selected staff
// authenticator. It never manufactures an actor from an HTTP field.
func (a routeAuthenticator) ResolveActor(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (httpapi.Actor, error) {
	if spec.Access == httpapi.Storefront || spec.Access == httpapi.BuyerCompany || spec.Access == httpapi.CustomerSession {
		return httpapi.Actor{}, nil
	}
	if resolver, ok := a.staff.(httpapi.ActorResolver); ok {
		return resolver.ResolveActor(ctx, r, spec)
	}
	return httpapi.Actor{}, nil
}

// ResolveTransactionAuthorization forwards only staff write admission. Storefront
// and buyer commands retain their capability checks and never acquire staff scope.
func (a routeAuthenticator) ResolveTransactionAuthorization(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (func(context.Context, *db.Queries) error, error) {
	if spec.Access == httpapi.Storefront || spec.Access == httpapi.BuyerCompany || spec.Access == httpapi.CustomerSession {
		return nil, nil
	}
	if resolver, ok := a.staff.(httpapi.TransactionAuthorizationResolver); ok {
		return resolver.ResolveTransactionAuthorization(ctx, r, spec)
	}
	return nil, nil
}
