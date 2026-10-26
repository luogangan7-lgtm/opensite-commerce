// Package observability exports process-local HTTP telemetry, using stdlib only.
// Bootstrap installs Wrap outside httpapi's panic recovery; scrapers call /metrics.
// It owns no tables. Labels never contain paths, tenant IDs, queries or identities.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

type requestKey struct {
	method string
	status int
}
type latency struct {
	count   uint64
	seconds float64
	buckets [10]uint64
}

var bounds = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// Metrics is scoped to one server lifetime. Its zero value is ready to use.
// ponytail: a short global lock covers counters; shard only if contention is measured.
type Metrics struct {
	mu            sync.Mutex
	requests      map[requestKey]uint64
	durations     map[string]latency
	auth          map[authKey]uint64
	authDurations map[string]latency
}

// Wrap must enclose the recovery transport so counters reflect the committed
// response, including a recovered panic's 500 rather than its discarded writes.
// /metrics is an operational endpoint, never a business RouteSpec.
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			if r.Method == http.MethodGet {
				m.write(w)
			}
			return
		}
		start := time.Now()
		response := &statusWriter{ResponseWriter: w}
		defer func() {
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			m.observe(r.Method, status, time.Since(start).Seconds())
		}()
		next.ServeHTTP(response, r.WithContext(context.WithValue(r.Context(), metricsKey{}, m)))
	})
}

func boundedMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

func (m *Metrics) observe(method string, status int, seconds float64) {
	method = boundedMethod(method)
	// net/http admits three-digit extension statuses; collapse them to a finite label.
	if status < 100 || status > 599 {
		status = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests = make(map[requestKey]uint64)
		m.durations = make(map[string]latency)
	}
	m.requests[requestKey{method, status}]++
	d := m.durations[method]
	d.count++
	d.seconds += seconds
	for i, bound := range bounds {
		if seconds <= bound {
			d.buckets[i]++
		}
	}
	m.durations[method] = d
}

func (m *Metrics) write(w http.ResponseWriter) {
	m.writeAuthentication(w)
	// Copy under lock: a slow scraper cannot block completed requests while writing.
	m.mu.Lock()
	requests := make(map[requestKey]uint64, len(m.requests))
	for k, v := range m.requests {
		requests[k] = v
	}
	durations := make(map[string]latency, len(m.durations))
	for k, v := range m.durations {
		durations[k] = v
	}
	m.mu.Unlock()
	keys := make([]requestKey, 0, len(requests))
	for k := range requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].status < keys[j].status
	})
	fmt.Fprint(w, "# HELP opensite_http_requests_total Completed HTTP requests, excluding metrics scrapes.\n# TYPE opensite_http_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(w, "opensite_http_requests_total{method=%q,status=%q} %d\n", k.method, fmt.Sprint(k.status), requests[k])
	}
	fmt.Fprint(w, "# HELP opensite_http_errors_total Completed HTTP responses with status 400 or higher.\n# TYPE opensite_http_errors_total counter\n")
	for _, k := range keys {
		if k.status >= 400 {
			fmt.Fprintf(w, "opensite_http_errors_total{method=%q,status=%q} %d\n", k.method, fmt.Sprint(k.status), requests[k])
		}
	}
	fmt.Fprint(w, "# HELP opensite_http_request_duration_seconds HTTP response duration, excluding metrics scrapes.\n# TYPE opensite_http_request_duration_seconds histogram\n")
	methods := make([]string, 0, len(durations))
	for method := range durations {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	for _, method := range methods {
		d := durations[method]
		for i, bound := range bounds {
			fmt.Fprintf(w, "opensite_http_request_duration_seconds_bucket{method=%q,le=%q} %d\n", method, fmt.Sprint(bound), d.buckets[i])
		}
		fmt.Fprintf(w, "opensite_http_request_duration_seconds_bucket{method=%q,le=\"+Inf\"} %d\n", method, d.count)
		fmt.Fprintf(w, "opensite_http_request_duration_seconds_sum{method=%q} %g\n", method, d.seconds)
		fmt.Fprintf(w, "opensite_http_request_duration_seconds_count{method=%q} %d\n", method, d.count)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusWriter) WriteHeader(status int) {
	// Interim 1xx responses do not commit the final status (except switching protocols).
	if status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.ResponseWriter.WriteHeader(status)
		w.status = status
	}
}
func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}
