// Healthcheck performs a bounded HTTP liveness probe using stdlib only.
// Container health commands call it; it owns no database connection or tables.
// Only the API's exact 204 sentinel succeeds; redirects and error bodies fail.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	address := "http://127.0.0.1:8080/healthz"
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "healthcheck: expected at most one URL")
		os.Exit(1)
	}
	if len(os.Args) == 2 {
		address = os.Args[1]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := check(ctx, address); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		os.Exit(1)
	}
}

func check(ctx context.Context, address string) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("invalid probe URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errors.New("invalid probe request")
	}
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("probe failed or timed out")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("expected status 204, got %d", response.StatusCode)
	}
	return nil
}
