// Package contract owns ledger's public posting and balance values and errors.
// Dependencies: money and stdlib; callers: authorized commerce modules and ledger.
// Invariants: exact positive amounts, one currency, balanced ordered entries;
// ledger owns ledger_accounts/ledger_transactions/ledger_entries, emits no events.
package contract

import (
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
	"strings"
	"unicode/utf8"
)

type Side string

const (
	Debit  Side = "debit"
	Credit Side = "credit"
)

var (
	ErrInvalid     = errors.New("invalid ledger input")
	ErrConflict    = errors.New("ledger idempotency content conflict")
	ErrNotFound    = errors.New("ledger account or transaction not found")
	ErrOverflow    = errors.New("ledger amount overflow")
	ErrUnavailable = errors.New("ledger storage unavailable")
	ErrForbidden   = errors.New("ledger tenant scope denied")
	ErrRetry       = errors.New("ledger transaction retry required")
)

type Entry struct {
	AccountID string
	Side      Side
	Amount    money.Money
}

// Entries are ordered: reordering changes content at the permanent source key.
type Transaction struct {
	TenantID, Currency, SourceType, SourceID, Event string
	Entries                                         []Entry
}
type Balance struct {
	Currency                   string
	Debits, Credits, NormalNet money.Money
}

// ValidID accepts the canonical nonzero UUID form used by tenant-scoped commands.
func ValidID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for i, c := range []byte(id) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (x Transaction) Validate() error {
	invalid := func(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }
	if !ValidID(x.TenantID) {
		return invalid("tenant UUID")
	}
	if err := (money.Money{Currency: x.Currency}).Validate(); err != nil {
		return invalid("currency")
	}
	for i, v := range []string{x.SourceType, x.SourceID, x.Event} {
		limit := 128
		if i == 1 {
			limit = 256
		}
		if !utf8.ValidString(v) || utf8.RuneCountInString(v) > limit || strings.TrimSpace(v) == "" || strings.IndexByte(v, 0) >= 0 {
			return invalid("source key")
		}
	}
	if len(x.Entries) < 2 || len(x.Entries) > 1000 {
		return invalid("entry count must be between 2 and 1000")
	}
	debit, credit := money.Money{Currency: x.Currency}, money.Money{Currency: x.Currency}
	for _, e := range x.Entries {
		if !ValidID(e.AccountID) {
			return invalid("account UUID")
		}
		if e.Amount.Amount <= 0 {
			return invalid("amount must be positive")
		}
		if err := e.Amount.Validate(); err != nil || e.Amount.Currency != x.Currency {
			return invalid("entry currency")
		}
		var err error
		switch e.Side {
		case Debit:
			debit, err = debit.Add(e.Amount)
		case Credit:
			credit, err = credit.Add(e.Amount)
		default:
			return invalid("entry side")
		}
		if err != nil {
			return ErrOverflow
		}
	}
	if debit.Amount != credit.Amount {
		return invalid("debits and credits must balance")
	}
	return nil
}
