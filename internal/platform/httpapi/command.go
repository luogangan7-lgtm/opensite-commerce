// Responsibilities: carry authorized command fingerprints and durable replies.
// Dependencies: C-04 protocol and caller-owned tenant transactions; callers:
// CMS and other command transports. Invariants: claim admission commits before
// work; completion shares the business commit, and no client supplies an actor.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

type CommandScope struct{ TenantID, ActorKind, ActorID, Operation, Key string }
type CommandCompletion func(context.Context, pgx.Tx, idempotency.Response) error
type CommandClaim func(context.Context, pgx.Tx, CommandScope, [32]byte, time.Duration, idempotency.ResponseStore) (CommandCompletion, *idempotency.Response, error)
type CommandConfig struct {
	Run   func(context.Context, string, func(context.Context, pgx.Tx) error) error
	Store idempotency.ResponseStore
	// TTL bounds recovery after a crashed/interrupted admitted request. The
	// provider and domain retain their permanent receipts across lease takeover.
	TTL   time.Duration
	Claim CommandClaim
}
type commandRequest struct {
	scope CommandScope
	hash  [32]byte
}
type commandRequestKey struct{}
type CommandSession struct {
	request  commandRequest
	complete CommandCompletion
	response idempotency.Response
}

// CaptureCommand runs inside Router admission and before strict decoding. It
// preserves the exact bounded body for C-04 while restoring it for the decoder.
func CaptureCommand(spec RouteSpec, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant, err := AuthorizedTenant(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		actor, err := AuthorizedActor(r.Context())
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		key, err := idempotency.ValidateKey(r.Header)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		var body []byte
		if r.Body != nil {
			limited := http.MaxBytesReader(w, r.Body, maxBodySize(r))
			body, err = io.ReadAll(limited)
			_ = limited.Close()
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					WriteProblem(w, r, apperrors.New(413, "request_too_large", "Request body exceeds the route limit"))
				} else {
					WriteProblem(w, r, apperrors.New(400, "invalid_request", "Request body cannot be read"))
				}
				return
			}
		}
		params := make(map[string]string)
		for _, part := range strings.Split(spec.Pattern, "/") {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
				params[name] = r.PathValue(name)
			}
		}
		hash, err := idempotency.FingerprintRequest(r, spec.Pattern, params, body)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		request := commandRequest{CommandScope{tenant, actor.Kind, actor.ID, spec.OperationID, key}, hash}
		ctx := context.WithValue(r.Context(), commandRequestKey{}, request)
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// BeginCommand is called after typed validation, so malformed requests never
// acquire a durable lease. Successful admission commits only the C-04 claim.
func BeginCommand(ctx context.Context, config CommandConfig, operation string) (*CommandSession, *idempotency.Response, error) {
	request, ok := ctx.Value(commandRequestKey{}).(commandRequest)
	if !ok || request.scope.Operation != operation {
		return nil, nil, apperrors.New(401, "unauthenticated", "Authorized command context is required")
	}
	tenant, err := AuthorizedTenant(ctx)
	if err != nil {
		return nil, nil, err
	}
	actor, err := AuthorizedActor(ctx)
	if err != nil {
		return nil, nil, err
	}
	if tenant != request.scope.TenantID || actor.Kind != request.scope.ActorKind || actor.ID != request.scope.ActorID {
		return nil, nil, apperrors.New(401, "unauthenticated", "Authorized command context changed")
	}
	if config.Run == nil || config.Store == nil {
		return nil, nil, apperrors.New(503, "idempotency_unavailable", "Command persistence is unavailable")
	}
	if config.TTL == 0 {
		config.TTL = 2 * time.Minute
	}
	claim := config.Claim
	if claim == nil {
		claim = claimCommandTx
	}
	session := &CommandSession{request: request}
	var replay *idempotency.Response
	err = config.Run(ctx, tenant, func(txctx context.Context, tx pgx.Tx) error {
		current, e := AuthorizedTenant(txctx)
		if e != nil || current != tenant {
			return apperrors.New(401, "unauthenticated", "Authorized transaction context is required")
		}
		currentActor, e := AuthorizedActor(txctx)
		if e != nil || currentActor != actor {
			return apperrors.New(401, "unauthenticated", "Authorized actor context is required")
		}
		session.complete, replay, e = claim(txctx, tx, request.scope, request.hash, config.TTL, config.Store)
		return e
	})
	if err != nil {
		return nil, nil, commandError(err)
	}
	if replay != nil {
		return nil, replay, nil
	}
	if session.complete == nil {
		return nil, nil, apperrors.New(503, "idempotency_unavailable", "Command persistence is unavailable")
	}
	return session, nil, nil
}

// Key separates permanent domain receipts by the same complete C-04 scope.
// The admitted lowercase UUID is retained in C-04; domain copies use UUIDv5.
func (s *CommandSession) Key() string {
	encoded, _ := json.Marshal(s.request.scope)
	return idempotency.DerivedKey("http.command.v1", string(encoded))
}

// Complete must run in the transaction containing the domain's final writes.
// A stale lease or unavailable durable store aborts those writes. The returned
// exact saved response can be emitted only after the caller's commit succeeds.
func (s *CommandSession) Complete(ctx context.Context, tx pgx.Tx, status int, value any) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		return err
	}
	return s.CompleteResponse(ctx, tx, idempotency.Response{Status: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body.Bytes()})
}

// CompleteResponse saves the exact response, including revision and cache
// headers, atomically with the domain changes. The durable store validates it.
func (s *CommandSession) CompleteResponse(ctx context.Context, tx pgx.Tx, response idempotency.Response) error {
	if s == nil || s.complete == nil {
		return apperrors.New(503, "idempotency_unavailable", "Command persistence is unavailable")
	}
	tenant, err := AuthorizedTenant(ctx)
	if err != nil || tenant != s.request.scope.TenantID {
		return apperrors.New(401, "unauthenticated", "Authorized transaction context is required")
	}
	actor, err := AuthorizedActor(ctx)
	if err != nil || actor.Kind != s.request.scope.ActorKind || actor.ID != s.request.scope.ActorID {
		return apperrors.New(401, "unauthenticated", "Authorized actor context is required")
	}
	if err := s.complete(ctx, tx, response); err != nil {
		return commandError(err)
	}
	s.response = response
	return nil
}
func (s *CommandSession) Response() idempotency.Response { return s.response }
func WriteCommandResponse(w http.ResponseWriter, response idempotency.Response) error {
	for name, values := range response.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.Status)
	_, err := w.Write(response.Body)
	return err
}
func commandError(err error) error {
	var public *apperrors.Error
	if errors.As(err, &public) {
		return public
	}
	return apperrors.New(503, "idempotency_unavailable", "Command persistence is unavailable; retry with the same key after the claim expires")
}
