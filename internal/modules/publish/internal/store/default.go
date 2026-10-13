//go:build !pgx

// Package store fails closed when the PostgreSQL backend is not compiled.
// Dependencies: publish contract; caller: facade; invariant: no simulated data.
package store

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
)

type Store struct{}

func Open(context.Context, string) (*Store, func(), error) { return nil, nil, contract.ErrUnavailable }
func (*Store) Current(context.Context, string) (string, string, string, []byte, []byte, service.Live, error) {
	return "", "", "", nil, nil, service.Live{}, contract.ErrUnavailable
}
