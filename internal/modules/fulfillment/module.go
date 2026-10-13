// Package fulfillment statically composes its private service for bootstrap.
// Dependencies: public contract and generated-query store; callers: admin API.
// Invariants: callers own authorized transactions; this module opens no pool.
package fulfillment

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment/internal/service"
)

func New() contract.Service { return service.Service{} }
