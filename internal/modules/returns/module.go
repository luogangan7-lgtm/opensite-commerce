// Package returns composes transaction commands and the two-phase refund port.
package returns

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/internal/store"
)

type Config = contract.Config

type Coordinator interface {
	contract.Service
	contract.RefundCompleter
}

func New(config Config) Coordinator {
	if config.RefundJournal != nil {
		config.RefundJournal = store.RefundJournal{Journal: config.RefundJournal}
	}
	return service.New(config, store.Repository{})
}
