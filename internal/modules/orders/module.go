// Package orders is the static composition entry point. It wires the private
// service to the public contract without owning a DB connection or importing
// payments. Checkout and trusted event consumers supply authorized pgx.Tx.
package orders

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/internal/service"
)

func New() contract.Service                                  { return service.Service{} }
func NewQuoteCreator() contract.QuoteCreator                 { return service.Service{} }
func NewReservationValidator() contract.ReservationValidator { return service.Service{} }
