// Package service freezes quote prices using existing exact tax/shipping calculators.
// Dependencies: public quote contract, money, tax, shipping; callers: quotes facade.
// Invariants: negotiated unit prices are accepted; no FX, overflow, or mutable rule aliases.
package service

import (
	"errors"
	"fmt"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"strings"
	"time"
	"unicode/utf8"
)

func Calculate(input c.CreateInput, rules c.CalculationRules, at time.Time) (c.Snapshot, error) {
	if !c.ValidID(input.SiteID) || !c.ValidID(input.InquiryID) || !c.ValidID(input.CompanyID) || at.IsZero() || !input.ExpiresAt.After(at) || len(input.Lines) == 0 || len(input.Lines) > 100 || input.WeightG < 0 || strings.TrimSpace(input.ShippingProfileID) == "" {
		return c.Snapshot{}, c.ErrInvalid
	}
	if (money.Money{Currency: input.Currency}).Validate() != nil {
		return c.Snapshot{}, c.ErrInvalid
	}
	items := make([]tax.LineItem, len(input.Lines))
	lines := make([]c.Line, len(input.Lines))
	for i, line := range input.Lines {
		if (line.ProductID != "" && !c.ValidID(line.ProductID)) || (line.VariantID != "" && (!c.ValidID(line.VariantID) || line.ProductID == "")) || line.UnitPrice < 0 || line.Quantity <= 0 || strings.TrimSpace(line.Description) == "" || !utf8.ValidString(line.Description) || utf8.RuneCountInString(line.Description) > 1000 || strings.ContainsRune(line.Description, 0) {
			return c.Snapshot{}, c.ErrInvalid
		}
		items[i] = tax.LineItem{ID: fmt.Sprint(i + 1), Currency: input.Currency, Category: line.TaxCategory, UnitAmount: line.UnitPrice, Quantity: line.Quantity}
		lines[i] = c.Line{LineNo: int32(i + 1), ProductID: line.ProductID, VariantID: line.VariantID, Description: line.Description, Quantity: line.Quantity, UnitPrice: line.UnitPrice}
	}
	taxed, err := tax.Calculate(input.Destination, items, rules.IncludeTax, at, rules.Tax)
	if err != nil {
		if errors.Is(err, money.ErrOverflow) {
			return c.Snapshot{}, c.ErrOverflow
		}
		return c.Snapshot{}, c.ErrInvalid
	}
	shipped, err := shipping.Quote(rules.ShippingZones, rules.ShippingRates, shipping.Request{ProfileID: input.ShippingProfileID, Destination: shipping.Destination{Country: input.Destination.Country, Subdivision: input.Destination.Subdivision}, WeightG: input.WeightG, Subtotal: money.Money{Amount: taxed.Totals.Net, Currency: input.Currency}})
	if err != nil {
		if errors.Is(err, money.ErrOverflow) {
			return c.Snapshot{}, c.ErrOverflow
		}
		return c.Snapshot{}, c.ErrInvalid
	}
	total, err := (money.Money{Amount: taxed.Totals.Gross, Currency: input.Currency}).Add(shipped.Charge)
	if err != nil {
		return c.Snapshot{}, c.ErrOverflow
	}
	for i := range lines {
		lines[i].Snapshot = taxed.Lines[i]
		lines[i].LineTotal = taxed.Lines[i].Amounts.Net
	}
	return c.Snapshot{Calculator: "quotes-v1;tax-line-half-up;shipping-v1", Currency: input.Currency, Lines: lines, Tax: taxed, Shipping: shipped, Subtotal: taxed.Totals.Net, TaxAmount: taxed.Totals.Tax, ShippingAmount: shipped.Charge.Amount, Total: total.Amount, CalculatedAt: at, Destination: input.Destination, ShippingProfileID: input.ShippingProfileID, WeightG: input.WeightG}, nil
}

// ValidateTransition mirrors the public state machine; SQL independently checks current members and role.
func ValidateTransition(q c.Quote, action c.Action, kind, id string, at time.Time) error {
	if kind != "staff" && kind != "customer" {
		return c.ErrUnauthorized
	}
	if action == c.Send || action == c.Withdraw || action == c.Expire {
		if kind != "staff" {
			return c.ErrForbidden
		}
	} else if action == c.Accept || action == c.Approve || action == c.Reject {
		if kind != "customer" {
			return c.ErrForbidden
		}
	} else {
		return c.ErrInvalid
	}
	if (action == c.Approve || action == c.Reject) && q.AcceptedBy == id {
		return c.ErrForbidden
	}
	switch action {
	case c.Send:
		if q.Status != "draft" || !at.Before(q.ExpiresAt) {
			return c.ErrConflict
		}
	case c.Accept:
		if q.Status != "sent" || !at.Before(q.ExpiresAt) {
			return c.ErrConflict
		}
	case c.Approve, c.Reject:
		if q.Status != "accepted_pending_approval" || !at.Before(q.ExpiresAt) {
			return c.ErrConflict
		}
	case c.Withdraw:
		if q.Status != "sent" && q.Status != "accepted_pending_approval" {
			return c.ErrConflict
		}
	case c.Expire:
		if (q.Status != "sent" && q.Status != "accepted_pending_approval") || at.Before(q.ExpiresAt) {
			return c.ErrConflict
		}
	}
	return nil
}
