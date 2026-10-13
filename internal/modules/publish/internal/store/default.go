//go:build !pgx

// Package store fails closed when the PostgreSQL backend is not compiled.
// Dependencies: publish contract; caller: facade; invariant: no simulated data.
package store

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
)

type Store struct{}

func Open(context.Context, string) (*Store, func(), error) { return nil, nil, contract.ErrUnavailable }
func (*Store) Serving(context.Context, string) (string, []byte, error) {
	return "", nil, contract.ErrUnavailable
}
func (*Store) Current(context.Context, string) (string, string, string, []byte, []byte, error) {
	return "", "", "", nil, nil, contract.ErrUnavailable
}
