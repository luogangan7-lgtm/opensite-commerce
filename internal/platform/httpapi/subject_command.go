// Responsibilities: subject-resolver command adapter used by C-22a privacy/CSV
// routes (CMS uses the C-04 CaptureCommand/BeginCommand path in command.go) to
// carry bounded raw mutation input into typed handlers and compose same-transaction idempotency. Dependencies: idempotency + caller's
// approved pgx runner; privacy/CSV adapters call it. Actor authority comes only
// from the configured validated-context resolver, never wire fields.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"io"
	"net/http"
	"time"
)

type Subject struct{ Kind, ID string }
type SubjectResolver func(context.Context) (Subject, error)
type TxRunner func(context.Context, string, func(context.Context, pgx.Tx) error) error
type SubjectCommandConfig struct {
	Run       TxRunner
	Subject   SubjectResolver
	Responses idempotency.ResponseStore
}

func RequireSubject(ctx context.Context, tenant string, resolve SubjectResolver) (Subject, error) {
	stamped, err := AuthorizedTenant(ctx)
	if err != nil {
		return Subject{}, err
	}
	if stamped != tenant {
		return Subject{}, apperrors.New(404, "not_found", "Resource not found")
	}
	if resolve == nil {
		return Subject{}, apperrors.New(401, "unauthenticated", "Validated subject is required")
	}
	subject, err := resolve(ctx)
	if err != nil {
		return Subject{}, apperrors.New(401, "unauthenticated", "Validated subject is required")
	}
	if !tenanttx.ValidTenantID(subject.ID) || (subject.Kind != "user" && subject.Kind != "api_key" && subject.Kind != "service") {
		return Subject{}, apperrors.New(401, "unauthenticated", "Validated subject is required")
	}
	return subject, nil
}
func Unavailable() error {
	return apperrors.New(503, "service_unavailable", "Configured persistence and worker capabilities are required")
}

type commandInputKey struct{}
type commandInput struct {
	request *http.Request
	body    []byte
	key     string
}

// AdmitCommand preserves exact raw body bytes for fingerprinting. Authentication
// has already run in Router; no request reaches a database callback here.
func AdmitCommand(route Route) Route {
	next := route.Handler
	route.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key, err := idempotency.ValidateKey(r.Header)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		limit := route.Spec.MaxBodySize
		if limit == 0 {
			limit = 1
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				WriteProblem(w, r, apperrors.New(413, "request_too_large", "Request body exceeds the route limit"))
			} else {
				WriteProblem(w, r, apperrors.New(400, "invalid_request", "Unable to read command"))
			}
			return
		}
		if route.Spec.MaxBodySize == 0 && len(raw) != 0 {
			WriteProblem(w, r, apperrors.New(400, "invalid_request", "This command accepts no body"))
			return
		}
		if int64(len(raw)) > limit {
			WriteProblem(w, r, apperrors.New(413, "request_too_large", "Request body exceeds the route limit"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), commandInputKey{}, commandInput{r, raw, key})))
	})
	return route
}
func Command(ctx context.Context, cfg SubjectCommandConfig, tenant, operation, pattern string, params map[string]string, fn func(context.Context, pgx.Tx, Subject) (idempotency.Response, error)) (idempotency.Response, error) {
	subject, err := RequireSubject(ctx, tenant, cfg.Subject)
	if err != nil {
		return idempotency.Response{}, err
	}
	input, ok := ctx.Value(commandInputKey{}).(commandInput)
	if !ok {
		return idempotency.Response{}, apperrors.New(400, "invalid_request", "Command admission is required")
	}
	if cfg.Run == nil || cfg.Responses == nil {
		return idempotency.Response{}, Unavailable()
	}
	hash, err := idempotency.FingerprintRequest(input.request, pattern, params, input.body)
	if err != nil {
		return idempotency.Response{}, err
	}
	var response idempotency.Response
	err = cfg.Run(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		response, err = executeSubjectCommand(ctx, tx, idempotency.Scope{TenantID: tenant, ActorKind: subject.Kind, ActorID: subject.ID, Operation: operation, Key: input.key}, hash, time.Minute, cfg.Responses, func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) { return fn(ctx, tx, subject) })
		return err
	})
	return response, err
}
func JSONResponse(status int, value any) (idempotency.Response, error) {
	body, err := json.Marshal(value)
	return idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body}, err
}
func WriteResponse(w http.ResponseWriter, response idempotency.Response) error {
	for name, values := range response.Header {
		w.Header()[name] = append([]string(nil), values...)
	}
	w.WriteHeader(response.Status)
	_, err := w.Write(response.Body)
	return err
}
