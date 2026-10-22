// Package returns composes transaction commands and the two-phase refund port.
// Dependencies: public contract and private service/store; callers: bootstrap.
// Invariants: missing captured-payment/account bindings fail closed; production
// must inject trusted readers, and paymentfake is only a test adapter.
package returns

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/service"
)

type Config = contract.Config

func New(config Config) contract.Service { return service.New(config) }
