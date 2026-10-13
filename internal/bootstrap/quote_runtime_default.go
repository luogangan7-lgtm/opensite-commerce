//go:build !pgx

// Responsibility: fail closed when optional quote persistence is not compiled.
// Dependencies: none; caller: explicit dev/test startup. No connection capability.
package bootstrap

import (
	"context"
	"errors"
)

func openQuoteRuntime(context.Context, string) (runtimeQuoteRunner, func(), error) {
	return runtimeQuoteRunner{}, nil, errors.New("quote checkout requires the pgx build capability")
}
