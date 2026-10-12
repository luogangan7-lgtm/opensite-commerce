// Cart HTTP adaptation belongs to the platform boundary, which alone imports
// generated production server types. It depends on cart's public Service port
// and is mounted by bootstrap. Only four cart operations mount; actor custody
// stays in module auth and strict admission precedes generated decoding.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

type CartHandler struct {
	generated.StrictServerInterface
	service contract.Service
}

func NewCartHandler(service contract.Service) *CartHandler { return &CartHandler{service: service} }

func (h *CartHandler) Routes() []Route {
	strict := generated.NewStrictHandlerWithOptions(h, nil, generated.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
			WriteProblem(w, r, apperrors.New(400, "invalid_json", "Invalid cart request"))
		},
		ResponseErrorHandlerFunc: WriteProblem,
	})
	wrapper := generated.ServerInterfaceWrapper{Handler: strict, ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
		WriteProblem(w, r, apperrors.New(400, "invalid_request", "Invalid cart request"))
	}}
	const base = "/api/v1/t/{tenant_id}/carts"
	return []Route{
		{Spec: RouteSpec{OperationID: "createCart", Method: "POST", Pattern: base, Access: Storefront, MaxBodySize: 4096}, Handler: h.admit("createCart", wrapper.CreateCart)},
		{Spec: RouteSpec{OperationID: "getCart", Method: "GET", Pattern: base + "/{cart_id}", Access: Storefront}, Handler: h.admit("getCart", wrapper.GetCart)},
		{Spec: RouteSpec{OperationID: "setCartLine", Method: "PUT", Pattern: base + "/{cart_id}/lines/{sku_id}", Access: Storefront, MaxBodySize: 4096}, Handler: h.admit("setCartLine", wrapper.SetCartLine)},
		{Spec: RouteSpec{OperationID: "deleteCartLine", Method: "DELETE", Pattern: base + "/{cart_id}/lines/{sku_id}", Access: Storefront}, Handler: h.admit("deleteCartLine", wrapper.DeleteCartLine)},
	}
}

func (h *CartHandler) admit(operation string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if operation == "setCartLine" || operation == "deleteCartLine" {
			values := r.Header.Values("If-Match")
			if len(values) == 0 || len(values) == 1 && values[0] == "" {
				WriteProblem(w, r, apperrors.New(428, "precondition_required", "If-Match is required"))
				return
			}
			if len(values) != 1 {
				WriteProblem(w, r, apperrors.New(412, "precondition_failed", "Exact strong If-Match required"))
				return
			}
		}
		if operation == "createCart" || operation == "setCartLine" {
			// Struct decoding matches keys case-insensitively. Admit the one
			// exact schema key before typed/generated decoding can fold names.
			var fields map[string]json.RawMessage
			if err := DecodeJSON(w, r, &fields); err != nil {
				WriteProblem(w, r, err)
				return
			}
			key := "currency"
			if operation == "setCartLine" {
				key = "quantity"
			}
			if len(fields) != 1 || fields[key] == nil {
				WriteProblem(w, r, apperrors.New(400, "invalid_json", "Body must contain exactly the canonical cart field"))
				return
			}
			body, err := json.Marshal(fields)
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			var dto any
			if operation == "createCart" {
				dto = &generated.CreateCart{}
			} else {
				dto = &struct {
					Quantity *int64 `json:"quantity"`
				}{}
			}
			if err := json.Unmarshal(body, dto); err != nil {
				WriteProblem(w, r, apperrors.New(400, "invalid_json", "Invalid cart field type"))
				return
			}
			if operation == "setCartLine" {
				quantity := dto.(*struct {
					Quantity *int64 `json:"quantity"`
				}).Quantity
				if quantity == nil || *quantity < 0 || *quantity > contract.MaxQuantity {
					WriteProblem(w, r, apperrors.New(400, "invalid_quantity", "Quantity must be an integer between zero and the cart limit"))
					return
				}
			}
			// Shared admission already rejected duplicate decoded keys, depth,
			// trailing JSON and wire size; only the canonical key reaches codegen.
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next(w, r)
	})
}

func cartActor(ctx context.Context, tenant string) (contract.AuthorizedActor, error) {
	a, ok := contract.Actor(ctx)
	if !ok || a.TenantID != tenant {
		return a, apperrors.New(404, "not_found", "Cart not found")
	}
	return a, nil
}

var cartCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func (h *CartHandler) CreateCart(ctx context.Context, r generated.CreateCartRequestObject) (generated.CreateCartResponseObject, error) {
	a, err := cartActor(ctx, r.TenantId.String())
	if err != nil {
		return nil, err
	}
	if r.Body == nil || !cartCurrencyPattern.MatchString(r.Body.Currency) {
		return nil, apperrors.New(400, "invalid_currency", "Currency must be three uppercase letters")
	}
	created, err := h.service.Create(ctx, a, r.Body.Currency)
	if err != nil {
		return nil, err
	}
	view, err := cartRepresentation(created.Cart)
	if err != nil {
		return nil, err
	}
	body := generated.CreatedCart{Cart: view}
	if created.CartToken != "" {
		body.CartToken = &created.CartToken
	}
	return generated.CreateCart201JSONResponse{Body: body, Headers: generated.CreateCart201ResponseHeaders{ETag: contract.ETag(created.Cart), CacheControl: "no-store"}}, nil
}
func (h *CartHandler) GetCart(ctx context.Context, r generated.GetCartRequestObject) (generated.GetCartResponseObject, error) {
	a, err := cartActor(ctx, r.TenantId.String())
	if err != nil {
		return nil, err
	}
	v, err := h.service.Get(ctx, a, r.CartId.String())
	if err != nil {
		return nil, err
	}
	body, err := cartRepresentation(v)
	if err != nil {
		return nil, err
	}
	return generated.GetCart200JSONResponse{Body: body, Headers: generated.GetCart200ResponseHeaders{ETag: contract.ETag(v), CacheControl: "no-store"}}, nil
}
func (h *CartHandler) SetCartLine(ctx context.Context, r generated.SetCartLineRequestObject) (generated.SetCartLineResponseObject, error) {
	a, err := cartActor(ctx, r.TenantId.String())
	if err != nil {
		return nil, err
	}
	if r.Body == nil || r.Body.Quantity < 0 || r.Body.Quantity > contract.MaxQuantity {
		return nil, apperrors.New(400, "invalid_quantity", "Quantity must be an integer between zero and the cart limit")
	}
	v, err := h.service.PutLine(ctx, a, r.CartId.String(), r.SkuId.String(), r.Body.Quantity, r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	body, err := cartRepresentation(v)
	if err != nil {
		return nil, err
	}
	return generated.SetCartLine200JSONResponse{Body: body, Headers: generated.SetCartLine200ResponseHeaders{ETag: contract.ETag(v), CacheControl: "no-store"}}, nil
}
func (h *CartHandler) DeleteCartLine(ctx context.Context, r generated.DeleteCartLineRequestObject) (generated.DeleteCartLineResponseObject, error) {
	a, err := cartActor(ctx, r.TenantId.String())
	if err != nil {
		return nil, err
	}
	v, err := h.service.DeleteLine(ctx, a, r.CartId.String(), r.SkuId.String(), r.Params.IfMatch)
	if err != nil {
		return nil, err
	}
	body, err := cartRepresentation(v)
	if err != nil {
		return nil, err
	}
	return generated.DeleteCartLine200JSONResponse{Body: body, Headers: generated.DeleteCartLine200ResponseHeaders{ETag: contract.ETag(v), CacheControl: "no-store"}}, nil
}
func cartRepresentation(v contract.View) (generated.Cart, error) {
	// Contract JSON field names are owned by cart; generated UUID models check
	// identifiers without a second manually-maintained field mapping.
	body, err := json.Marshal(v)
	if err != nil {
		return generated.Cart{}, err
	}
	var result generated.Cart
	err = json.Unmarshal(body, &result)
	return result, err
}
