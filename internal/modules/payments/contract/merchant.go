// Responsibility: Payments-owned provider settlement authority port.
// Dependencies: the caller's transaction-scoped generated query set; callers:
// the dormant Payments deletion gate. No network IO is permitted under the fence.
// Invariant: absent, invalid or unconfirmed authority never proves settlement.
package contract

import (
	"context"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
)

type SettlementOutcome string

const (
	SettlementSettled   SettlementOutcome = "settled"
	SettlementUnsettled SettlementOutcome = "unsettled"
	SettlementUnknown   SettlementOutcome = "unknown"
)

type ProviderSettlement interface {
	Settlement(context.Context, *db.Queries, string) (SettlementOutcome, error)
}
