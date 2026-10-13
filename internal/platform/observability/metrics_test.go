// Telemetry tests use concurrent local requests and inspect the public scrape.
// They depend on stdlib and observability only; bounded labels are the invariant.
package observability_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/observability"
)

func TestConcurrentScrapesAndBoundedLabels(t *testing.T) {
	var metrics observability.Metrics
	handler := metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(418); w.WriteHeader(201) }))
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(fmt.Sprintf("CUSTOM%d", i), fmt.Sprintf("/secret%d?password=secret", i), nil))
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/metrics", nil))
		}(i)
	}
	wg.Wait()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{`opensite_http_requests_total{method="OTHER",status="418"} 100`, `opensite_http_errors_total{method="OTHER",status="418"} 100`, `opensite_http_request_duration_seconds_bucket{method="OTHER",le="+Inf"} 100`, `opensite_http_request_duration_seconds_count{method="OTHER"} 100`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, forbidden := range []string{"secret", "CUSTOM", `status="201"`, `method="GET"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("unexpected %q in metrics", forbidden)
		}
	}
}

func TestScrapeMethodsAndImplicitStatus(t *testing.T) {
	var metrics observability.Metrics
	handler := metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/item", nil))
	for _, tc := range []struct {
		method string
		status int
	}{{"HEAD", 200}, {"POST", 405}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, "/metrics", nil))
		if w.Code != tc.status || w.Body.Len() != 0 {
			t.Fatalf("method=%s status=%d body=%q", tc.method, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `opensite_http_requests_total{method="POST",status="200"} 1`) {
		t.Fatal(w.Body.String())
	}
}

func TestWorkerOutboxRuntimeNOT_RUN(t *testing.T) {
	t.Skip("NOT_RUN: worker/outbox polling awaits merged reliability implementation; WorkerMetrics is an interface only")
}

func TestLatencyMeasuresElapsedTime(t *testing.T) {
	var metrics observability.Metrics
	handler := metrics.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(10 * time.Millisecond); w.WriteHeader(204) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/item", nil))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, `opensite_http_request_duration_seconds_sum{method="GET"} `) {
			parts := strings.Fields(line)
			seconds, err := strconv.ParseFloat(parts[1], 64)
			if err != nil || seconds < 0.005 {
				t.Fatalf("elapsed metric=%s", line)
			}
			return
		}
	}
	t.Fatal("missing measured duration")
}
