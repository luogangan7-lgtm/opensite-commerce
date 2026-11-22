// Package checkout exposes the checkout orchestrator to static composition.
package checkout

import (
	cart "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/internal/store"
)

type Config = contract.Config

func New(config Config) contract.ServiceWithPaymentEvents { return service.New(config) }

// NewSQLCart reads shopping intent inside the checkout transaction.
func NewSQLCart(read cart.IntentReader) contract.CartReader { return store.NewCart(read) }

// RuleMetadata only supplies canonical catalog fields not modeled by this base.
type RuleMetadata = store.RuleMetadata
type SKUMetadata = store.SKUMetadata

func NewSQLRules(metadata RuleMetadata) contract.RulesReader { return store.NewRules(metadata) }
