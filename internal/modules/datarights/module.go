// Package datarights composes private service/store through its public contract.
// Depends on local filesystem and authorized transaction callback; called by
// bootstrap. Export files are separate from media originals and private.
package datarights

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/internal/service"
)

func New(c contract.Config) (contract.Service, error) { return service.New(c) }
