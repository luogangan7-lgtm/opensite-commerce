// These tests exercise the public money contract with exact big-integer oracles.
// Dependencies are test-only standard library; no database or HTTP is involved.
// Invariants: boundary errors, deterministic rounding/allocation, no input mutation.
package money

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"testing"
)

func TestValidate(t *testing.T) {
	for _, c := range []string{"USD", "TWD", "ZZZ", "ABC"} {
		if err := (Money{math.MinInt64, c}).Validate(); err != nil {
			t.Errorf("%q: %v", c, err)
		}
	}
	for _, c := range []string{"", "US", "USDD", "usd", "Usd", "US1", " US", "UŠD", "美元", "US\x00"} {
		if err := (Money{0, c}).Validate(); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("%q: %v", c, err)
		}
	}
}

func TestArithmetic(t *testing.T) {
	tests := []struct {
		name, op   string
		a, b, want int64
		err        error
	}{
		{"add", "add", 12, -2, 10, nil}, {"addMax", "add", math.MaxInt64, 0, math.MaxInt64, nil},
		{"addMin", "add", math.MinInt64, 0, math.MinInt64, nil}, {"addHigh", "add", math.MaxInt64, 1, 0, ErrOverflow},
		{"addLow", "add", math.MinInt64, -1, 0, ErrOverflow}, {"cancel", "add", math.MinInt64, math.MaxInt64, -1, nil},
		{"sub", "sub", 12, 2, 10, nil}, {"subMin", "sub", math.MinInt64, math.MinInt64, 0, nil},
		{"subMax", "sub", math.MaxInt64, math.MaxInt64, 0, nil}, {"subHigh", "sub", 0, math.MinInt64, 0, ErrOverflow},
		{"subLow", "sub", math.MinInt64, 1, 0, ErrOverflow}, {"subMinusOne", "sub", -1, math.MinInt64, math.MaxInt64, nil},
		{"mul", "mul", -4, 3, -12, nil}, {"mulMin", "mul", math.MinInt64, 1, math.MinInt64, nil},
		{"mulMax", "mul", math.MaxInt64, 1, math.MaxInt64, nil}, {"mulZero", "mul", math.MinInt64, 0, 0, nil},
		{"mulHigh", "mul", math.MaxInt64, 2, 0, ErrOverflow}, {"mulLow", "mul", math.MinInt64, 2, 0, ErrOverflow},
		{"mulNegative", "mul", 3, -1, 0, ErrInvalidQuantity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, b := Money{tt.a, "USD"}, Money{tt.b, "USD"}
			var got Money
			var err error
			switch tt.op {
			case "add":
				got, err = m.Add(b)
			case "sub":
				got, err = m.Sub(b)
			case "mul":
				got, err = m.Mul(tt.b)
			}
			if !errors.Is(err, tt.err) || got.Currency != "USD" || (err == nil && got.Amount != tt.want) {
				t.Fatalf("got %+v, %v; want %d, %v", got, err, tt.want, tt.err)
			}
		})
	}
	for _, op := range []string{"add", "sub"} {
		for _, pair := range [][2]string{{"USD", "EUR"}, {"usd", "USD"}, {"USD", "usd"}} {
			m, b := Money{1, pair[0]}, Money{1, pair[1]}
			var got Money
			var err error
			if op == "add" {
				got, err = m.Add(b)
			} else {
				got, err = m.Sub(b)
			}
			want := ErrInvalidCurrency
			if pair[0] == "USD" && pair[1] == "EUR" {
				want = ErrCurrencyMismatch
			}
			if !errors.Is(err, want) || got.Currency != m.Currency {
				t.Fatalf("%s %v: %+v %v", op, pair, got, err)
			}
		}
	}
	bad := Money{1, "usd"}
	if _, err := bad.Mul(1); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := bad.Scale(1, 1); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
	if _, err := bad.Allocate([]int64{1}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatal(err)
	}
}

func TestScale(t *testing.T) {
	tests := []struct {
		name          string
		a, n, d, want int64
		err           error
	}{
		{"positiveHalf", 1, 1, 2, 1, nil}, {"negativeHalf", -1, 1, 2, -1, nil},
		{"positiveNonHalf", 5, 1, 3, 2, nil}, {"negativeNonHalf", -5, 1, 3, -2, nil},
		{"belowHalf", 1, 1, 3, 0, nil}, {"negativeBelowHalf", -1, 1, 3, 0, nil},
		{"minIdentity", math.MinInt64, math.MaxInt64, math.MaxInt64, math.MinInt64, nil},
		{"maxIdentity", math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64, nil},
		{"minHalf", math.MinInt64, 1, 2, math.MinInt64 / 2, nil}, {"maxHalf", math.MaxInt64, 1, 2, 1 << 62, nil},
		{"zeroNumerator", math.MinInt64, 0, 1, 0, nil}, {"overflow", math.MaxInt64, 2, 1, 0, ErrOverflow},
		{"negativeOverflow", math.MinInt64, 2, 1, 0, ErrOverflow}, {"negativeN", 1, -1, 2, 0, ErrInvalidRatio},
		{"zeroD", 1, 1, 0, 0, ErrInvalidRatio}, {"negativeD", 1, 1, -1, 0, ErrInvalidRatio},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (Money{tt.a, "TWD"}).Scale(tt.n, tt.d)
			if !errors.Is(err, tt.err) || got.Currency != "TWD" || (err == nil && got.Amount != tt.want) {
				t.Fatalf("%+v %v, want %d %v", got, err, tt.want, tt.err)
			}
		})
	}
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name          string
		a             int64
		weights, want []int64
		err           error
	}{
		{"tie", 5, []int64{1, 1, 1}, []int64{2, 2, 1}, nil},
		{"negativeTie", -5, []int64{1, 1, 1}, []int64{-2, -2, -1}, nil},
		{"remainders", 7, []int64{1, 2, 3}, []int64{1, 2, 4}, nil},
		{"zeroWeight", 5, []int64{0, 1, 0, 1}, []int64{0, 3, 0, 2}, nil},
		{"zeroAmount", 0, []int64{1, 2}, []int64{0, 0}, nil},
		{"minSingle", math.MinInt64, []int64{1}, []int64{math.MinInt64}, nil},
		{"minTie", math.MinInt64, []int64{1, 1, 1}, []int64{-3074457345618258603, -3074457345618258603, -3074457345618258602}, nil},
		{"largeWeights", math.MaxInt64, []int64{math.MaxInt64, math.MaxInt64}, []int64{1 << 62, (1 << 62) - 1}, nil},
		{"negativeLargeWeights", math.MinInt64, []int64{math.MaxInt64, math.MaxInt64}, []int64{math.MinInt64 / 2, math.MinInt64 / 2}, nil},
		{"empty", 3, nil, nil, ErrInvalidWeights}, {"allZero", 3, []int64{0, 0}, nil, ErrInvalidWeights},
		{"negative", 3, []int64{2, -1}, nil, ErrInvalidWeights},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]int64(nil), tt.weights...)
			m := Money{tt.a, "ZZZ"}
			got, err := m.Allocate(tt.weights)
			if !errors.Is(err, tt.err) {
				t.Fatalf("error %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(before, tt.weights) {
				t.Fatal("weights mutated")
			}
			if err != nil {
				return
			}
			amounts := make([]int64, len(got))
			for i, v := range got {
				amounts[i] = v.Amount
				if v.Currency != "ZZZ" {
					t.Fatal(v)
				}
			}
			if !reflect.DeepEqual(amounts, tt.want) {
				t.Fatalf("%v, want %v", amounts, tt.want)
			}
			again, err := m.Allocate(tt.weights)
			if err != nil || !reflect.DeepEqual(got, again) {
				t.Fatal("nondeterministic")
			}
		})
	}
}

func TestRandomExactOracles(t *testing.T) {
	rng := rand.New(rand.NewSource(1012))
	edge := []int64{math.MinInt64, math.MaxInt64, -1, 0, 1, -2, 2}
	value := func(i int) int64 {
		if i%3 == 0 {
			return edge[rng.Intn(len(edge))]
		}
		return int64(rng.Uint64())
	}
	for i := 0; i < 700; i++ {
		a, b := value(i), value(i+1)
		m := Money{a, "USD"}
		n := b & math.MaxInt64
		if i%4 == 0 {
			n = int64(rng.Intn(5))
		}
		add, ae := m.Add(Money{b, "USD"})
		sub, se := m.Sub(Money{b, "USD"})
		mul, me := m.Mul(n)
		checkExact(t, add, ae, new(big.Int).Add(big.NewInt(a), big.NewInt(b)))
		checkExact(t, sub, se, new(big.Int).Sub(big.NewInt(a), big.NewInt(b)))
		checkExact(t, mul, me, new(big.Int).Mul(big.NewInt(a), big.NewInt(n)))
		d := int64(rng.Uint64()>>1) + 1
		if d <= 0 {
			d = 1
		}
		r := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(a), big.NewInt(n)), big.NewInt(d))
		mag := new(big.Rat).Abs(r)
		mag.Add(mag, big.NewRat(1, 2))
		rounded := new(big.Int).Quo(mag.Num(), mag.Denom())
		if a < 0 {
			rounded.Neg(rounded)
		}
		scaled, err := m.Scale(n, d)
		checkExact(t, scaled, err, rounded)
		weights := make([]int64, 1+rng.Intn(10))
		weights[0] = 1
		for j := 1; j < len(weights); j++ {
			weights[j] = int64(rng.Uint64() >> 1)
			if j%3 == 0 {
				weights[j] = 0
			}
		}
		before := append([]int64(nil), weights...)
		parts, err := m.Allocate(weights)
		if err != nil {
			t.Fatal(err)
		}
		again, err := m.Allocate(weights)
		if err != nil || !reflect.DeepEqual(parts, again) || !reflect.DeepEqual(before, weights) {
			t.Fatal("allocation nondeterminism or mutation")
		}
		total := new(big.Int)
		for _, w := range weights {
			total.Add(total, big.NewInt(w))
		}
		sum := new(big.Int)
		floors := make([]*big.Int, len(weights))
		rems := make([]*big.Int, len(weights))
		baseSum := new(big.Int)
		abs := new(big.Int).Abs(big.NewInt(a))
		for j, w := range weights {
			floors[j] = new(big.Int)
			rems[j] = new(big.Int)
			floors[j].QuoRem(new(big.Int).Mul(abs, big.NewInt(w)), total, rems[j])
			baseSum.Add(baseSum, floors[j])
		}
		left := new(big.Int).Sub(abs, baseSum).Int64()
		for j, p := range parts {
			if p.Currency != "USD" || (a >= 0 && p.Amount < 0) || (a <= 0 && p.Amount > 0) || (weights[j] == 0 && p.Amount != 0) {
				t.Fatalf("bad allocation %+v", parts)
			}
			sum.Add(sum, big.NewInt(p.Amount))
			actual := new(big.Int).Abs(big.NewInt(p.Amount))
			ceil := new(big.Int).Set(floors[j])
			if rems[j].Sign() > 0 {
				ceil.Add(ceil, big.NewInt(1))
			}
			if actual.Cmp(floors[j]) != 0 && actual.Cmp(ceil) != 0 {
				t.Fatal("not floor/ceil")
			}
			rank := 0
			for k := range weights {
				cmp := rems[k].Cmp(rems[j])
				if cmp > 0 || (cmp == 0 && k < j) {
					rank++
				}
			}
			expected := new(big.Int).Set(floors[j])
			if int64(rank) < left {
				expected.Add(expected, big.NewInt(1))
			}
			if a < 0 {
				expected.Neg(expected)
			}
			if !expected.IsInt64() || p.Amount != expected.Int64() {
				t.Fatalf("incorrect remainder rank %d: %v expected %v", j, p, expected)
			}
		}
		if sum.Cmp(big.NewInt(a)) != 0 {
			t.Fatal("sum not conserved")
		}
	}
}

func checkExact(t *testing.T, got Money, err error, want *big.Int) {
	t.Helper()
	if got.Currency != "USD" {
		t.Fatal("currency lost")
	}
	if !want.IsInt64() {
		if !errors.Is(err, ErrOverflow) {
			t.Fatalf("%v: expected overflow, got %+v %v", want, got, err)
		}
		return
	}
	if err != nil || got.Amount != want.Int64() {
		t.Fatalf("expected %v, got %+v %v", want, got, err)
	}
}
