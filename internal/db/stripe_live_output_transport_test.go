//go:build pgx && stripe_live && stripe_live_offline

// Responsibility: compile an exclusively fake transport into normal CI's
// output-boundary binary. This file is absent from the owner/live build.
// Caller: test_stripe_live_process.py through the actual production wrapper.
package db_test

import (
	"fmt"
	"net/http"
	"os"
)

func init(){
	// These real process writes must remain inside the wrapper's private pipe.
	fmt.Fprintln(os.Stdout,"https://invoice.example.test/private-canary owner-canary@example.test pi_full_private_identifier")
	fmt.Fprintln(os.Stderr,"https://invoice.example.test/private-canary owner-canary@example.test in_full_private_identifier")
	if os.Getenv("STRIPE_LIVE_OFFLINE_CASE")=="panic"{panic("https://invoice.example.test/private-canary owner-canary@example.test re_full_private_identifier")}
	http.DefaultTransport=&livePGTransport{scenario:os.Getenv("STRIPE_LIVE_OFFLINE_CASE")}
}
