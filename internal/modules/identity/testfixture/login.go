// Package testfixture supplies pure registration fixtures for bootstrap contracts.
// Dependencies: identity's private production transport and platform HTTP ports.
// Called only by contract tests; no database, keys or production startup wiring.
// Invariant: callers inspect registrations and never invoke IO handlers.
package testfixture

import (
	transport "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/httpapi"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"time"
)

// LoginHandler returns the real transport for offline registration contracts.
func LoginHandler(origin string, idle time.Duration) interface {
	Routes() []platform.Route
	platform.Authenticator
} {
	return transport.New(nil, origin, idle)
}
