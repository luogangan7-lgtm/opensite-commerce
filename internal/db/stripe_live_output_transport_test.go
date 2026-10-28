//go:build pgx && stripe_live && stripe_live_offline

// Responsibility: compile fake transport into normal CI output-boundary binary.
// Dependencies: fake provider; caller: actual public wrapper process canaries.
// This file is excluded from the owner build; raw writes stay in a private pipe.
package db_test

import (
	"fmt"
	"net/http"
	"os"
)

func init() {
	fmt.Fprintln(os.Stdout, "https://invoice.example.test/private-canary owner-canary@example.test pi_full_private_identifier")
	fmt.Fprintln(os.Stderr, "https://invoice.example.test/private-canary owner-canary@example.test in_full_private_identifier")
	if os.Getenv("STRIPE_LIVE_OFFLINE_CASE") == "panic" {
		panic("https://invoice.example.test/private-canary owner-canary@example.test re_full_private_identifier")
	}
	http.DefaultTransport = &livePGTransport{scenario: os.Getenv("STRIPE_LIVE_OFFLINE_CASE")}
}
