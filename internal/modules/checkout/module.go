// Package checkout exposes the single checkout orchestrator to static composition.
// Dependencies: only checkout's private service and public contract; no pools,
// HTTP auth or provider selection. Missing ports remain a default-deny boundary.
package checkout

import (
	cart "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
)

type Config = contract.Config

func New(config Config) contract.Service { return service.New(config) }

// NewSQLCart consumes the cart module's public transaction-scoped intent reader.
// Static composition supplies cart.ReadIntent; checkout imports only its contract.
func NewSQLCart(read cart.IntentReader) contract.CartReader { return store.NewCart(read) }

// RuleMetadata only supplies canonical catalog fields not modeled by this base.
type RuleMetadata = store.RuleMetadata
type SKUMetadata = store.SKUMetadata

func NewSQLRules(metadata RuleMetadata) contract.RulesReader { return store.NewRules(metadata) }
