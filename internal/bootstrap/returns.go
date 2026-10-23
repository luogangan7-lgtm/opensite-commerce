// Responsibility: bind Returns only to the isolated coordinator and Payments.
// Dependencies: public refund ports; caller: FromEnvironment after all providers.
// Invariants: no implicit provider, no runtime role borrowing or client money proof.
package bootstrap

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/payments"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	returnscontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

type returnsReaders struct{ journal payments.RefundJournal }

func (r returnsReaders) CapturedPayment(ctx context.Context, tx pgx.Tx, tenant, order string) (returnscontract.CapturedPayment, error) {
	c, err := r.journal.PaidContext(ctx, tx, order)
	return returnscontract.CapturedPayment{TenantID: tenant, OrderID: order, ProviderRef: c.ProviderRef, OrderVersion: c.PaymentVersion, Paid: money.Money{Amount: c.AmountMinor, Currency: c.Currency}}, err
}
func (r returnsReaders) RefundAccounts(ctx context.Context, tx pgx.Tx, tenant, currency string) (returnscontract.RefundAccounts, error) {
	c, err := r.journal.RefundAccounts(ctx, tx, currency)
	return returnscontract.RefundAccounts{Income: c.Accounts["revenue"], Tax: c.Accounts["tax_payable"], Shipping: c.Accounts["shipping_revenue"], Cash: c.Accounts["cash"]}, err
}
func wireReturns(c *Config) {
	if c.ReturnsCoordinator.Runner == nil || c.Payment == nil {
		return
	}
	journal := payments.RefundJournal{Ledger: ledger.New()}
	c.ReturnsCoordinator.Payments, c.ReturnsCoordinator.Ledger = c.Payment, journal
	c.ReturnsCoordinator.Captures = returnsReaders{journal}
	c.ReturnsCoordinator.Accounts = returnsReaders{journal}
	c.Returns.Service = returns.New(c.ReturnsCoordinator)
}
