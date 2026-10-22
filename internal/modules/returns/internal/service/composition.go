// Responsibilities: bind the production sqlc repository to refund composition.
// Dependencies: public contract/private store; callers: returns module entry.
// Invariants: no connection ownership or fake provider registration here.
package service

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/store"
)

func New(config contract.Config) *Service { return &Service{config: config, repo: store.Repository{}} }
