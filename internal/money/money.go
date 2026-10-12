// Package money owns exact integer amount arithmetic; it depends only on the
// standard library, owns no tables, and is called by commerce pricing consumers.
// Invariants: currency is three uppercase ASCII letters; no implicit FX, float,
// input mutation, or silent overflow; allocation conserves the signed amount.
package money

import (
	"errors"
	"math/big"
	"sort"
)

type Money struct {
	Amount   int64
	Currency string
}

var (
	ErrInvalidCurrency  = errors.New("invalid currency")
	ErrCurrencyMismatch = errors.New("currency mismatch")
	ErrOverflow         = errors.New("amount overflow")
	ErrInvalidQuantity  = errors.New("invalid quantity")
	ErrInvalidWeights   = errors.New("invalid weights")
	ErrInvalidRatio     = errors.New("invalid ratio")
)

func (m Money) Validate() error {
	if len(m.Currency) != 3 {
		return ErrInvalidCurrency
	}
	for i := 0; i < 3; i++ {
		if m.Currency[i] < 'A' || m.Currency[i] > 'Z' {
			return ErrInvalidCurrency
		}
	}
	return nil
}

func (m Money) compatible(b Money) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if m.Currency != b.Currency {
		return ErrCurrencyMismatch
	}
	return nil
}

func (m Money) result(amount *big.Int) (Money, error) {
	result := Money{Currency: m.Currency}
	if !amount.IsInt64() {
		return result, ErrOverflow
	}
	result.Amount = amount.Int64()
	return result, nil
}

func (m Money) Add(b Money) (Money, error) {
	if err := m.compatible(b); err != nil {
		return Money{Currency: m.Currency}, err
	}
	return m.result(new(big.Int).Add(big.NewInt(m.Amount), big.NewInt(b.Amount)))
}

func (m Money) Sub(b Money) (Money, error) {
	if err := m.compatible(b); err != nil {
		return Money{Currency: m.Currency}, err
	}
	return m.result(new(big.Int).Sub(big.NewInt(m.Amount), big.NewInt(b.Amount)))
}

func (m Money) Mul(quantity int64) (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{Currency: m.Currency}, err
	}
	if quantity < 0 {
		return Money{Currency: m.Currency}, ErrInvalidQuantity
	}
	return m.result(new(big.Int).Mul(big.NewInt(m.Amount), big.NewInt(quantity)))
}

// Scale rounds exact halves away from zero and checks only the final amount.
func (m Money) Scale(numerator, denominator int64) (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{Currency: m.Currency}, err
	}
	if numerator < 0 || denominator <= 0 {
		return Money{Currency: m.Currency}, ErrInvalidRatio
	}
	product := new(big.Int).Mul(big.NewInt(m.Amount), big.NewInt(numerator))
	product.Abs(product)
	quotient, remainder := new(big.Int), new(big.Int)
	divisor := big.NewInt(denominator)
	quotient.QuoRem(product, divisor, remainder)
	if remainder.Lsh(remainder, 1).Cmp(divisor) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if m.Amount < 0 {
		quotient.Neg(quotient)
	}
	return m.result(quotient)
}

// Allocate distributes absolute units by largest remainder, then restores sign.
// Stable order makes equal remainders favor the lower input index.
func (m Money) Allocate(weights []int64) ([]Money, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	total := new(big.Int)
	for _, weight := range weights {
		if weight < 0 {
			return nil, ErrInvalidWeights
		}
		total.Add(total, big.NewInt(weight))
	}
	if total.Sign() == 0 {
		return nil, ErrInvalidWeights
	}
	amount := new(big.Int).Abs(big.NewInt(m.Amount))
	left := new(big.Int).Set(amount)
	parts := make([]*big.Int, len(weights))
	remainders := make([]*big.Int, len(weights))
	order := make([]int, len(weights))
	for i, weight := range weights {
		parts[i], remainders[i] = new(big.Int), new(big.Int)
		parts[i].QuoRem(new(big.Int).Mul(amount, big.NewInt(weight)), total, remainders[i])
		left.Sub(left, parts[i])
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return remainders[order[i]].Cmp(remainders[order[j]]) > 0 })
	// The leftover is smaller than len(weights), even for MinInt64 and huge totals.
	for i := int64(0); i < left.Int64(); i++ {
		parts[order[i]].Add(parts[order[i]], big.NewInt(1))
	}
	result := make([]Money, len(weights))
	for i, part := range parts {
		if m.Amount < 0 {
			part.Neg(part)
		}
		result[i] = Money{Amount: part.Int64(), Currency: m.Currency}
	}
	return result, nil
}
