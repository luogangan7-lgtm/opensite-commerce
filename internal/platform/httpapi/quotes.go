// Responsibilities: bind quote/inquiry routes to the generated strict server.
// Dependencies: quotes contract, generated DTOs, shared strict JSON and router;
// bootstrap calls Routes. Owns no tables, SQL, transactions or authentication IO.
// Invariants: domain calls require a sealed staff/customer/verified-host stamp;
// client identity headers/body never grant authority, writes require CAS+key.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	gen "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

// QuotesService is implemented by the quotes application transaction facade.
type QuotesService interface {
	SubmitInquiry(context.Context, qc.InquiryInput) (qc.Inquiry, error)
	Create(context.Context, qc.CreateInput, qc.WriteOptions) (qc.Quote, error)
	Get(context.Context, string) (qc.Quote, error)
	Transition(context.Context, string, qc.Action, qc.WriteOptions) (qc.Quote, error)
}

// QuotesInquiryResolver authenticates a verified hostname-to-tenant/site mapping
// by returning contract.WithTrustedInquiry context; nil is always denied.
type QuotesInquiryResolver func(context.Context, *http.Request) (context.Context, error)

type QuotesHandler struct {
	gen.StrictServerInterface // Only our explicit Routes below can reach this adapter.
	service                   QuotesService
	buyer                     Authenticator
	inquiry                   QuotesInquiryResolver
	limiter                   inquiryLimiter
}

func NewQuotesHandler(service QuotesService, buyer Authenticator, inquiry QuotesInquiryResolver) *QuotesHandler {
	return &QuotesHandler{service: service, buyer: buyer, inquiry: inquiry, limiter: inquiryLimiter{entries: make(map[string]inquiryWindow)}}
}
func (h *QuotesHandler) Routes() []Route {
	fail := func(w http.ResponseWriter, r *http.Request, e error) { WriteProblem(w, r, quoteError(e)) }
	strict := gen.NewStrictHandlerWithOptions(h, nil, gen.StrictHTTPServerOptions{RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) { WriteProblem(w, r, invalidJSON()) }, ResponseErrorHandlerFunc: fail})
	wrapper := gen.ServerInterfaceWrapper{Handler: strict, ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
		WriteProblem(w, r, apperrors.New(400, "invalid_request", "Invalid request parameter"))
	}}
	return []Route{
		{Spec: RouteSpec{OperationID: "submitInquiry", Method: "POST", Pattern: "/api/public/v1/inquiries", Access: Public, MaxBodySize: 64 << 10}, Handler: h.admit("submitInquiry", wrapper.SubmitInquiry)},
		{Spec: RouteSpec{OperationID: "createQuote", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/quotes", Access: AdminOrAPIKey, Scope: "quotes:write", MaxBodySize: 128 << 10}, Handler: h.admit("createQuote", wrapper.CreateQuote)},
		{Spec: RouteSpec{OperationID: "getQuote", Method: "GET", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}", Access: AdminOrAPIKey, Scope: "quotes:read"}, Handler: h.admit("getQuote", wrapper.GetQuote)},
		{Spec: RouteSpec{OperationID: "sendQuote", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}/send", Access: AdminOrAPIKey, Scope: "quotes:write", MaxBodySize: 1024}, Handler: h.admit("sendQuote", wrapper.SendQuote)},
		{Spec: RouteSpec{OperationID: "withdrawQuote", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}/withdraw", Access: AdminOrAPIKey, Scope: "quotes:write", MaxBodySize: 1024}, Handler: h.admit("withdrawQuote", wrapper.WithdrawQuote)},
		{Spec: RouteSpec{OperationID: "expireQuote", Method: "POST", Pattern: "/api/v1/t/{tenant_id}/quotes/{quote_id}/expire", Access: AdminOrAPIKey, Scope: "quotes:write", MaxBodySize: 1024}, Handler: h.admit("expireQuote", wrapper.ExpireQuote)},
		{Spec: RouteSpec{OperationID: "getBuyerQuote", Method: "GET", Pattern: "/api/public/v1/quotes/{quote_id}", Access: Public}, Handler: h.admit("getBuyerQuote", wrapper.GetBuyerQuote)},
		{Spec: RouteSpec{OperationID: "acceptQuote", Method: "POST", Pattern: "/api/public/v1/quotes/{quote_id}/accept", Access: Public, MaxBodySize: 1024}, Handler: h.admit("acceptQuote", wrapper.AcceptQuote)},
		{Spec: RouteSpec{OperationID: "approveQuote", Method: "POST", Pattern: "/api/public/v1/quotes/{quote_id}/approve", Access: Public, MaxBodySize: 1024}, Handler: h.admit("approveQuote", wrapper.ApproveQuote)},
		{Spec: RouteSpec{OperationID: "rejectQuote", Method: "POST", Pattern: "/api/public/v1/quotes/{quote_id}/reject", Access: Public, MaxBodySize: 1024}, Handler: h.admit("rejectQuote", wrapper.RejectQuote)},
	}
}
func (h *QuotesHandler) admit(op string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx := r.Context()
		var err error
		switch op {
		case "submitInquiry":
			if h.inquiry == nil {
				WriteProblem(w, r, quoteError(qc.ErrUnauthorized))
				return
			}
			ctx, err = h.inquiry(ctx, r)
			if err == nil && ctx != nil {
				_, err = qc.InquiryFromContext(ctx)
			} else if err == nil {
				err = qc.ErrUnauthorized
			}
			if err == nil && !h.limiter.allow(r.RemoteAddr, time.Now()) {
				w.Header().Set("Retry-After", "60")
				err = apperrors.New(429, "rate_limited", "Inquiry attempt limit exceeded")
			}
		case "getBuyerQuote", "acceptQuote", "approveQuote", "rejectQuote":
			if h.buyer == nil {
				WriteProblem(w, r, quoteError(qc.ErrUnauthorized))
				return
			}
			ctx, err = h.buyer.Authenticate(ctx, r, RouteSpec{OperationID: op, Access: Public})
			if err == nil && ctx != nil {
				a, e := qc.ActorFromContext(ctx)
				err = e
				if err == nil && a.Kind() != "customer" {
					err = qc.ErrUnauthorized
				}
			} else if err == nil {
				err = qc.ErrUnauthorized
			}
		default:
			a, e := qc.ActorFromContext(ctx)
			err = e
			if err == nil && a.Kind() != "staff" {
				err = qc.ErrUnauthorized
			}
			if err == nil && a.TenantID() != r.PathValue("tenant_id") {
				err = qc.ErrNotFound
			}
		}
		if err != nil {
			WriteProblem(w, r, quoteError(err))
			return
		}
		r = r.WithContext(ctx)
		if r.Method == http.MethodPost && op != "submitInquiry" {
			if err = quotePreconditions(r, op == "createQuote"); err != nil {
				WriteProblem(w, r, err)
				return
			}
		}
		if r.Method == http.MethodPost {
			var dto any
			schema := "QuoteTransition"
			switch op {
			case "createQuote":
				dto = &gen.CreateQuote{}
				schema = "CreateQuote"
			case "submitInquiry":
				dto = &gen.CreateInquiry{}
				schema = "CreateInquiry"
			default:
				dto = &gen.QuoteTransition{}
			}
			var body bytes.Buffer
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(r.Body, &body), r.Body}
			if err = DecodeJSON(w, r, dto); err == nil {
				err = quoteSchemaAdmission(schema, body.Bytes())
			}
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
		}
		if h.service == nil {
			WriteProblem(w, r, quoteError(qc.ErrUnavailable))
			return
		}
		next(w, r)
	})
}

var quoteVersion = regexp.MustCompile(`^"quote-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-v[1-9][0-9]*"$`)

func quotePreconditions(r *http.Request, create bool) error {
	match := r.Header.Values("If-Match")
	if len(match) == 0 || len(match) == 1 && strings.TrimSpace(match[0]) == "" {
		return apperrors.New(428, "precondition_required", "If-Match is required")
	}
	if len(match) != 1 || create && match[0] != "*" || !create && !quoteVersion.MatchString(match[0]) {
		return apperrors.New(412, "etag_mismatch", "A single strong current quote ETag is required")
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 || len(key[0]) == 0 || len(key[0]) > 128 || strings.TrimSpace(key[0]) != key[0] || strings.IndexFunc(key[0], func(c rune) bool { return c < 0x21 || c > 0x7e }) >= 0 {
		return apperrors.New(400, "invalid_idempotency_key", "One visible ASCII Idempotency-Key of at most 128 bytes is required")
	}
	return nil
}
func quoteSchemaAdmission(name string, body []byte) error {
	doc, err := gen.GetSwagger()
	if err != nil {
		return err
	}
	var value any
	if json.Unmarshal(body, &value) != nil || doc.Components.Schemas[name].Value.VisitJSON(value) != nil {
		return invalidJSON()
	}
	return nil
}
func quoteError(err error) error {
	for _, v := range []struct {
		sentinel error
		status   int
		code     string
	}{{qc.ErrUnauthorized, 401, "unauthenticated"}, {qc.ErrForbidden, 403, "forbidden"}, {qc.ErrNotFound, 404, "not_found"}, {qc.ErrInvalid, 422, "validation_failed"}, {qc.ErrOverflow, 422, "amount_overflow"}, {qc.ErrPreconditionRequired, 428, "precondition_required"}, {qc.ErrPrecondition, 412, "etag_mismatch"}, {qc.ErrConflict, 409, "quote_conflict"}, {qc.ErrIdempotencyReused, 422, "idempotency_key_reused"}, {qc.ErrUnavailable, 500, "internal_error"}} {
		if errors.Is(err, v.sentinel) {
			return apperrors.New(v.status, v.code, http.StatusText(v.status))
		}
	}
	return err
}

// ponytail: per-process fixed window, at most 10k remote IPs; use a shared
// durable limiter when deploying multiple API processes. Forwarded IPs are ignored.
type inquiryWindow struct {
	until time.Time
	count int
}
type inquiryLimiter struct {
	mu      sync.Mutex
	entries map[string]inquiryWindow
}

func (l *inquiryLimiter) allow(remote string, now time.Time) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	v, exists := l.entries[host]
	if !exists && len(l.entries) >= 10000 {
		for k, old := range l.entries {
			if !now.Before(old.until) {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= 10000 {
			return false
		}
	}
	if !now.Before(v.until) {
		v = inquiryWindow{until: now.Add(time.Minute)}
	}
	if v.count >= 5 {
		return false
	}
	v.count++
	l.entries[host] = v
	return true
}
func quoteRepresentation(q qc.Quote) (gen.Quote, error) {
	var out gen.Quote
	body, err := json.Marshal(q)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(body, &out)
	return out, err
}
func (h *QuotesHandler) SubmitInquiry(ctx context.Context, r gen.SubmitInquiryRequestObject) (gen.SubmitInquiryResponseObject, error) {
	site, err := qc.InquiryFromContext(ctx)
	if err != nil {
		return nil, err
	}
	in := qc.InquiryInput{SiteID: site.SiteID, Email: string(r.Body.BuyerEmail), Message: r.Body.Message, Quantity: r.Body.Quantity}
	if r.Body.BuyerName != nil {
		in.Name = *r.Body.BuyerName
	}
	if r.Body.ProductId != nil {
		in.ProductID = r.Body.ProductId.String()
	}
	receipt, err := h.service.SubmitInquiry(ctx, in)
	if err != nil {
		return nil, err
	}
	// Generated receipt only exposes id/status, never contact fields or host context.
	var dto gen.InquiryReceipt
	body, err := json.Marshal(struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{receipt.ID, receipt.Status})
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(body, &dto); err != nil {
		return nil, err
	}
	return gen.SubmitInquiry202JSONResponse(dto), nil
}
func (h *QuotesHandler) CreateQuote(ctx context.Context, r gen.CreateQuoteRequestObject) (gen.CreateQuoteResponseObject, error) {
	var in qc.CreateInput
	body, err := json.Marshal(r.Body)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	q, err := h.service.Create(ctx, in, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.CreateQuote201JSONResponse{Body: dto, Headers: gen.CreateQuote201ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) GetQuote(ctx context.Context, r gen.GetQuoteRequestObject) (gen.GetQuoteResponseObject, error) {
	q, err := h.service.Get(ctx, r.QuoteId.String())
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.GetQuote200JSONResponse{Body: dto, Headers: gen.GetQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) GetBuyerQuote(ctx context.Context, r gen.GetBuyerQuoteRequestObject) (gen.GetBuyerQuoteResponseObject, error) {
	q, err := h.service.Get(ctx, r.QuoteId.String())
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.GetBuyerQuote200JSONResponse{Body: dto, Headers: gen.GetBuyerQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) SendQuote(ctx context.Context, r gen.SendQuoteRequestObject) (gen.SendQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Send, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.SendQuote200JSONResponse{Body: dto, Headers: gen.SendQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) WithdrawQuote(ctx context.Context, r gen.WithdrawQuoteRequestObject) (gen.WithdrawQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Withdraw, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.WithdrawQuote200JSONResponse{Body: dto, Headers: gen.WithdrawQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) ExpireQuote(ctx context.Context, r gen.ExpireQuoteRequestObject) (gen.ExpireQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Expire, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.ExpireQuote200JSONResponse{Body: dto, Headers: gen.ExpireQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) AcceptQuote(ctx context.Context, r gen.AcceptQuoteRequestObject) (gen.AcceptQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Accept, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.AcceptQuote200JSONResponse{Body: dto, Headers: gen.AcceptQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) ApproveQuote(ctx context.Context, r gen.ApproveQuoteRequestObject) (gen.ApproveQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Approve, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.ApproveQuote200JSONResponse{Body: dto, Headers: gen.ApproveQuote200ResponseHeaders{ETag: &etag}}, nil
}
func (h *QuotesHandler) RejectQuote(ctx context.Context, r gen.RejectQuoteRequestObject) (gen.RejectQuoteResponseObject, error) {
	q, err := h.service.Transition(ctx, r.QuoteId.String(), qc.Reject, qc.WriteOptions{IfMatch: r.Params.IfMatch, IdempotencyKey: r.Params.IdempotencyKey})
	if err != nil {
		return nil, err
	}
	dto, err := quoteRepresentation(q)
	if err != nil {
		return nil, err
	}
	etag := q.ETag()
	return gen.RejectQuote200JSONResponse{Body: dto, Headers: gen.RejectQuote200ResponseHeaders{ETag: &etag}}, nil
}
