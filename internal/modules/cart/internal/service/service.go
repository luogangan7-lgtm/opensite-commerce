// Package service owns cart intent validation and live price projections.
// It depends on cart ports, money/pricing and stdlib; the module's HTTP adapter
// calls it. It never reserves inventory, creates orders or persists prices.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/cart/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/pricing"
)

type Service struct{ repo contract.Repository }

func New(repo contract.Repository) *Service { return &Service{repo: repo} }

func (s *Service) Create(ctx context.Context, actor contract.AuthorizedActor, currency string) (contract.Created, error) {
	if !validActor(actor, true) {
		return contract.Created{}, notFound()
	}
	if err := (money.Money{Currency: currency}).Validate(); err != nil {
		return contract.Created{}, apperrors.New(422, "invalid_currency", "Currency must be an uppercase three-letter code")
	}
	if s.repo == nil {
		return contract.Created{}, unavailable()
	}
	var token string
	if actor.CustomerID == "" {
		var bytes [32]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return contract.Created{}, err
		}
		token = base64.RawURLEncoding.EncodeToString(bytes[:])
		hash := sha256.Sum256([]byte(token))
		actor.GuestTokenHash = hash[:]
	}
	snapshot, err := s.repo.Create(ctx, actor, currency)
	if err != nil {
		return contract.Created{}, err
	}
	view, err := Project(snapshot)
	return contract.Created{Cart: view, CartToken: token}, err
}

func (s *Service) Get(ctx context.Context, actor contract.AuthorizedActor, id string) (contract.View, error) {
	if !validActor(actor, false) || !ValidID(id) {
		return contract.View{}, notFound()
	}
	if s.repo == nil {
		return contract.View{}, unavailable()
	}
	snapshot, err := s.repo.Read(ctx, actor, strings.ToLower(id))
	if err != nil {
		return contract.View{}, err
	}
	if snapshot.TenantID != actor.TenantID {
		return contract.View{}, notFound()
	}
	return Project(snapshot)
}

func (s *Service) PutLine(ctx context.Context, actor contract.AuthorizedActor, id, sku string, quantity int64, match string) (contract.View, error) {
	if !validActor(actor, false) || !ValidID(id) {
		return contract.View{}, notFound()
	}
	if !ValidID(sku) {
		return contract.View{}, apperrors.New(422, "invalid_sku", "SKU must be a UUID")
	}
	if quantity < 0 || quantity > contract.MaxQuantity {
		return contract.View{}, apperrors.New(422, "invalid_quantity", "Quantity must be between zero and 1000000")
	}
	if match == "" {
		return contract.View{}, apperrors.New(428, "precondition_required", "If-Match is required")
	}
	if s.repo == nil {
		return contract.View{}, unavailable()
	}
	check := func(snapshot contract.Snapshot) error {
		if snapshot.TenantID != actor.TenantID {
			return notFound()
		}
		view, err := Project(snapshot)
		if err != nil {
			return err
		}
		if match != contract.ETag(view) {
			return apperrors.New(412, "precondition_failed", "Cart representation has changed")
		}
		return nil
	}
	// Pricing and subtotal overflow are checked before commit. A rejected future
	// quantity cannot persist merely because it overflowed while making the reply.
	validate := func(snapshot contract.Snapshot) error {
		view, err := Project(snapshot)
		if err == nil && quantity > 0 && view.PricingError != "" {
			return apperrors.New(422, view.PricingError, "A complete current price is unavailable")
		}
		return err
	}
	snapshot, err := s.repo.Change(ctx, actor, strings.ToLower(id), strings.ToLower(sku), quantity, check, validate)
	if err != nil {
		return contract.View{}, err
	}
	return Project(snapshot)
}

func (s *Service) DeleteLine(ctx context.Context, actor contract.AuthorizedActor, id, sku, match string) (contract.View, error) {
	return s.PutLine(ctx, actor, id, sku, 0, match)
}

func Project(snapshot contract.Snapshot) (contract.View, error) {
	view := contract.View{ID: snapshot.ID, Currency: snapshot.Currency, Version: snapshot.Version, Lines: []contract.Line{}}
	total := money.Money{Currency: snapshot.Currency}
	lines := append([]contract.StoredLine(nil), snapshot.Lines...)
	sort.Slice(lines, func(i, j int) bool { return lines[i].SKUID < lines[j].SKUID })
	for _, line := range lines {
		quote, err := pricing.Quote(snapshot.PriceLists, pricing.Request{TenantID: snapshot.TenantID, SKUID: line.SKUID, Currency: snapshot.Currency, Quantity: line.Quantity, At: snapshot.At})
		if err != nil {
			failure := "price_unavailable"
			if errors.Is(err, money.ErrOverflow) {
				failure = "amount_overflow"
			}
			view.PricingError = failure
			view.Lines = append(view.Lines, contract.Line{SKUID: line.SKUID, Quantity: line.Quantity, AvailableQuantity: line.AvailableQuantity, PricingError: failure})
			continue
		}
		next, err := total.Add(quote.Line)
		if err != nil {
			view.PricingError = "amount_overflow"
		} else {
			total = next
		}
		view.Lines = append(view.Lines, contract.Line{SKUID: line.SKUID, Quantity: line.Quantity, UnitAmount: quote.Unit.Amount, LineAmount: quote.Line.Amount, PriceListID: quote.PriceListID, Priority: quote.Priority, MinQuantity: quote.MinQuantity, AvailableQuantity: line.AvailableQuantity, Sellable: line.Active && line.AvailableQuantity >= line.Quantity})
	}
	if view.PricingError == "" {
		view.Subtotal = total.Amount
	}
	return view, nil
}

func ValidID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil
}

func validActor(a contract.AuthorizedActor, anonymous bool) bool {
	if !ValidID(a.TenantID) || strings.ToLower(a.TenantID) != a.TenantID {
		return false
	}
	if a.CustomerID != "" {
		return ValidID(a.CustomerID) && len(a.GuestTokenHash) == 0
	}
	return len(a.GuestTokenHash) == 32 || anonymous && len(a.GuestTokenHash) == 0
}
func notFound() error { return apperrors.New(404, "cart_not_found", "Cart not found") }
func unavailable() error {
	return apperrors.New(503, "cart_unavailable", "Cart storage is unavailable")
}
