// Snapshot preparation binds caller-authorized pricing, tax and shipping
// results and emits immutable JSON for the generated order store. Integer-money
// arithmetic rejects currency mismatch, overflow and stale/incorrect lines.
package service

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
)

type prepared struct {
	lines, shipping, guest, adjustments []byte
	total                               int64
	ttl                                 int32
}
type lineSnapshot struct {
	ID             string `json:"id"`
	SKUID          string `json:"sku_id"`
	SKU            string `json:"sku"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	LineNetMinor   int64  `json:"line_net_minor"`
	LineTaxMinor   int64  `json:"line_tax_minor"`
	LineTotalMinor int64  `json:"line_total_minor"`
	// Pricing provenance is bound into the request hash (not order_lines columns).
	PriceListID   string                `json:"price_list_id"`
	PricePriority int32                 `json:"price_priority"`
	PriceMinQty   int64                 `json:"price_min_quantity"`
	QuoteSource   *contract.QuoteSource `json:"quote_source,omitempty"`
	ReservationID string                `json:"reservation_id,omitempty"`
	TaxSnapshot   tax.Snapshot          `json:"tax_snapshot"`
}

func validID(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u != uuid.Nil && len(id) == 36
}
func validKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if c < '!' || c > '~' {
			return false
		}
	}
	return true
}
func validTaxCategory(category string) bool {
	if len(category) < 1 || len(category) > 64 || !utf8.ValidString(category) {
		return false
	}
	// Same contract as tax.Category: ^[a-z][a-z0-9_]{0,63}$.
	if category[0] < 'a' || category[0] > 'z' {
		return false
	}
	for _, c := range category {
		if c != '_' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
func prepare(in contract.CreateInput) (prepared, error) {
	return prepareWithSource(in, false)
}

func prepareQuoted(in contract.CreateInput) (prepared, error) {
	return prepareWithSource(in, true)
}

func prepareWithSource(in contract.CreateInput, negotiated bool) (prepared, error) {
	invalid := func(reason string) (prepared, error) {
		return prepared{}, fmt.Errorf("%w: %s", contract.ErrInvalid, reason)
	}
	if !validID(in.ID) || !validKey(in.IdempotencyKey) || len(in.Lines) == 0 || len(in.Lines) > 1000 || len(in.Adjustments) > 1000 {
		return invalid("order identity or lines")
	}
	if negotiated && len(in.Adjustments) != 0 {
		return invalid("negotiated adjustments must already be in the quote")
	}
	base := money.Money{Currency: in.Currency}
	if err := base.Validate(); err != nil {
		return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
	}
	customer := in.Buyer.CustomerID != ""
	if customer {
		if !validID(in.Buyer.CustomerID) || in.Buyer.Guest != (contract.Contact{}) {
			return invalid("buyer")
		}
	} else {
		email, err := mail.ParseAddress(in.Buyer.Guest.Email)
		if err != nil || email.Address != in.Buyer.Guest.Email || len(in.Buyer.Guest.Email) > 254 || len(in.Buyer.Guest.Name) > 200 || len(in.Buyer.Guest.Phone) > 64 || !utf8.ValidString(in.Buyer.Guest.Name) || !utf8.ValidString(in.Buyer.Guest.Phone) {
			return invalid("guest contact")
		}
	}
	ttl := in.ReservationTTLSeconds
	if ttl == 0 {
		ttl = 900
	}
	if ttl < 1 || ttl > 86400 {
		return invalid("reservation TTL")
	}
	if in.Shipping.Charge.Amount < 0 || !validID(in.Shipping.ZoneID) || !validID(in.Shipping.RateID) || (in.Shipping.Method != "fixed" && in.Shipping.Method != "by_weight" && in.Shipping.Method != "free_over_amount") {
		return invalid("shipping quote")
	}
	total, err := base.Add(in.Shipping.Charge)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
	}
	lines := make([]lineSnapshot, 0, len(in.Lines))
	ids := map[string]bool{}
	reservations := map[string]bool{}
	quoteLines := map[int32]bool{}
	var quoteID string
	var quoteVersion int64
	for _, l := range in.Lines {
		id := strings.ToLower(l.ID)
		rid := strings.ToLower(l.ReservationID)
		if !validID(id) || ids[id] || !validID(l.SKUID) || l.Quantity <= 0 || strings.TrimSpace(l.SKU) == "" || !utf8.ValidString(l.SKU) || len(l.SKU) > 255 {
			return invalid("line identity or quantity")
		}
		ids[id] = true
		if rid != "" {
			if !validID(rid) || reservations[rid] {
				return invalid("duplicate/invalid reservation")
			}
			reservations[rid] = true
		}
		if negotiated {
			if l.QuoteSource == nil || !validID(l.QuoteSource.QuoteID) || l.QuoteSource.Version <= 0 || l.QuoteSource.LineNo <= 0 || quoteLines[l.QuoteSource.LineNo] || l.Price.PriceListID != "" || l.Price.Priority != 0 || l.Price.MinQuantity != 0 {
				return invalid("negotiated quote source")
			}
			source := *l.QuoteSource
			source.QuoteID = strings.ToLower(source.QuoteID)
			if quoteID != "" && (source.QuoteID != quoteID || source.Version != quoteVersion) {
				return invalid("mixed negotiated quotes")
			}
			quoteID, quoteVersion = source.QuoteID, source.Version
			quoteLines[source.LineNo] = true
			l.QuoteSource = &source
		} else if l.QuoteSource != nil || !validID(l.Price.PriceListID) || l.Price.MinQuantity < 1 || l.Price.MinQuantity > l.Quantity {
			return invalid("price-list source")
		}
		if l.Price.Unit.Amount < 0 || l.Price.Unit.Currency != in.Currency || l.Price.Line.Currency != in.Currency {
			return invalid("pricing quote")
		}
		priced, err := l.Price.Unit.Mul(l.Quantity)
		if err != nil {
			return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
		}
		if priced != l.Price.Line {
			return invalid("price quantity mismatch")
		}
		a, s := l.Tax.Amounts, l.Tax.Snapshot
		if l.Tax.ID != l.ID || s.Currency != in.Currency || a.Net < 0 || a.Tax < 0 || a.Gross < 0 || s.Rounding != "half_up" || !validTaxCategory(s.Category) || !validID(s.ZoneID) {
			return invalid("tax result")
		}
		if s.Reason == "rate" {
			if !validID(s.RateID) || s.BasisPoints < 0 || s.BasisPoints > 10000 || s.ValidFrom == nil || s.ValidFrom.IsZero() || (s.ValidUntil != nil && !s.ValidUntil.After(*s.ValidFrom)) {
				return invalid("tax rate snapshot")
			}
			from := s.ValidFrom.UTC()
			s.ValidFrom = &from
			if s.ValidUntil != nil {
				until := s.ValidUntil.UTC()
				s.ValidUntil = &until
			}
		} else if s.Reason == "exempt" {
			if s.RateID != "" || s.BasisPoints != 0 || s.ValidFrom != nil || s.ValidUntil != nil || a.Tax != 0 {
				return invalid("exempt tax snapshot")
			}
		} else {
			return invalid("tax reason")
		}
		gross, err := (money.Money{Amount: a.Net, Currency: in.Currency}).Add(money.Money{Amount: a.Tax, Currency: in.Currency})
		if err != nil {
			return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
		}

		denominator := int64(10000)
		if s.IncludeTax {
			denominator += s.BasisPoints
		}
		expectedTax, err := priced.Scale(s.BasisPoints, denominator)
		if err != nil {
			return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
		}
		if expectedTax.Amount != a.Tax {
			return invalid("tax rate amount binding")
		}
		priceBasis := a.Net
		if s.IncludeTax {
			priceBasis = a.Gross
		}
		if gross.Amount != a.Gross || priced.Amount != priceBasis {
			return invalid("tax price binding")
		}
		total, err = total.Add(gross)
		if err != nil {
			return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
		}
		lines = append(lines, lineSnapshot{ID: id, SKUID: strings.ToLower(l.SKUID), SKU: l.SKU, Quantity: l.Quantity, UnitPriceMinor: l.Price.Unit.Amount, LineNetMinor: a.Net, LineTaxMinor: a.Tax, LineTotalMinor: a.Gross, PriceListID: strings.ToLower(l.Price.PriceListID), PricePriority: l.Price.Priority, PriceMinQty: l.Price.MinQuantity, QuoteSource: l.QuoteSource, ReservationID: rid, TaxSnapshot: s})
	}
	for _, a := range in.Adjustments {
		id := strings.ToLower(a.ID)
		if !validID(id) || ids[id] || strings.TrimSpace(a.Reason) == "" || len(a.Reason) > 500 || !utf8.ValidString(a.Reason) {
			return invalid("adjustment")
		}
		ids[id] = true
		if a.Kind != "discount" && a.Kind != "surcharge" {
			return invalid("adjustment kind")
		}
		if a.Kind == "discount" && a.AmountMinor >= 0 || a.Kind == "surcharge" && a.AmountMinor <= 0 {
			return invalid("adjustment sign")
		}
		total, err = total.Add(money.Money{Amount: a.AmountMinor, Currency: in.Currency})
		if err != nil {
			return prepared{}, fmt.Errorf("%w: %w", contract.ErrInvalid, err)
		}
	}
	if total.Amount < 0 || (negotiated && total.Amount == 0) {
		return invalid("negative total")
	}
	linesJSON, err := json.Marshal(lines)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: tax serialization", contract.ErrInvalid)
	}
	shippingJSON, err := json.Marshal(in.Shipping)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: shipping serialization", contract.ErrInvalid)
	}
	guestJSON, err := json.Marshal(in.Buyer.Guest)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: guest serialization", contract.ErrInvalid)
	}
	adjustments := in.Adjustments
	if adjustments == nil {
		adjustments = []contract.Adjustment{}
	}
	adjustmentsJSON, err := json.Marshal(adjustments)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: adjustments serialization", contract.ErrInvalid)
	}
	return prepared{lines: linesJSON, shipping: shippingJSON, guest: guestJSON, adjustments: adjustmentsJSON, total: total.Amount, ttl: ttl}, nil
}
