package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
)

// LoadPaymentReceiptSnapshot reads the applied state in the tenant transaction
// holding the checkout lock shared with Confirm and Cancel (C-19a,
// docs/architecture/modules/PAYMENTS_FAKE.md).
func LoadPaymentReceiptSnapshot(ctx context.Context, tx pgx.Tx, tenantID, checkoutID string, status payment.PaymentStatus) (contract.Result, []byte, error) {
	row, err := db.New(tx).GetCheckoutByID(ctx, db.GetCheckoutByIDParams{TenantID: UUID(tenantID), ID: UUID(checkoutID)})
	if err != nil {
		return contract.Result{}, nil, MapError(err)
	}
	return contract.Result{
		CheckoutID: row.ID.String(), OrderID: row.OrderID.String(), State: row.Phase,
		AmountMinor: row.AmountMinor, Currency: row.Currency, OrderVersion: row.PayableVersion,
		Payment: contract.Intent{ProviderRef: row.ProviderRef, Status: status},
	}, row.FinalResult, nil
}
