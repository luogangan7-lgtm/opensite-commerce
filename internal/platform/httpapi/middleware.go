// Middleware owns request correlation, safe slog receipts and panic recovery.
// It uses stdlib and platform/clock; Router invokes it before routing/auth.
// Responses are committed only after success, so a panic cannot leak partial
// bytes or cookies. No credentials, query values or panic payloads are logged.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/clock"
)

type requestIDKey struct{}

func RequestID(ctx context.Context) string { id, _ := ctx.Value(requestIDKey{}).(string); return id }

func middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var entropy [16]byte
		_, _ = rand.Read(entropy[:])
		id := hex.EncodeToString(entropy[:])
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		start := clock.Now()
		response := &bufferedResponse{header: make(http.Header)}
		defer func() {
			if recover() != nil {
				response = &bufferedResponse{header: make(http.Header)}
				WriteProblem(response, r, nil)
				logger.ErrorContext(r.Context(), "http panic recovered", "request_id", id)
			}
			if response.status == 0 {
				response.status = 200
			}
			response.header.Set("X-Request-ID", id)
			for key, values := range response.header {
				w.Header()[key] = values
			}
			w.WriteHeader(response.status)
			_, _ = w.Write(response.body.Bytes())
			logger.InfoContext(r.Context(), "http request", "request_id", id, "method", r.Method, "pattern", r.Pattern, "status", response.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(response, r)
	})
}

// ponytail: this JSON skeleton buffers at most 1 MiB per response. Streaming,
// hijacking and larger downloads need a separate transport in their own card.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if status < 200 || status > 599 {
		panic("unsupported response status")
	}
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	// Same contract as net/http: sniff the type from the first write when unset.
	if w.body.Len() == 0 && w.header.Get("Content-Type") == "" && w.header.Get("Transfer-Encoding") == "" && len(body) > 0 {
		w.header.Set("Content-Type", http.DetectContentType(body))
	}
	if len(body) > (1<<20)-w.body.Len() {
		panic("response limit exceeded")
	}
	return w.body.Write(body)
}
