// Responsibilities: prove refund recovery and trust fences without a database.
// Dependencies: private service, public ports and Payment Fake; callers: Go gate.
// Invariants: a PSP side effect is never repeated when journal commit fails;
// mutable account/capture readers are not consulted after durable preparation.
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	ledger "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/ledger/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/money"
)

const tenantID = "11111111-1111-4111-8111-111111111111"
const orderID = "22222222-2222-4222-8222-222222222222"
const returnID = "33333333-3333-4333-8333-333333333333"
const account1 = "44444444-4444-4444-8444-444444444444"
const account2 = "55555555-5555-4555-8555-555555555555"
const account3 = "66666666-6666-4666-8666-666666666666"
const account4 = "77777777-7777-4777-8777-777777777777"
const journalID = "88888888-8888-4888-8888-888888888888"

// A local neutral-port test double keeps business tests inside their declared
// contract boundary. The real Payment Fake is exercised by the PG integration
// test outside this module. Only the three operations this fixture uses exist.
type paymentTestPort struct {
	captured payment.CreatePaymentIntent
	requests map[string]payment.PaymentOperation
	events   []payment.PaymentOperation
}

func newPaymentTestPort() *paymentTestPort {
	return &paymentTestPort{requests: make(map[string]payment.PaymentOperation)}
}
func (p *paymentTestPort) CreateIntent(_ context.Context, in payment.CreatePaymentIntent) (payment.PaymentIntentResult, error) {
	p.captured = in
	p.events = append(p.events, payment.PaymentOperation{CreatePaymentIntent: in})
	return payment.PaymentIntentResult{ProviderRef: "fixture-captured", Status: payment.StatusAuthorized}, nil
}
func (p *paymentTestPort) Capture(_ context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	p.events = append(p.events, in)
	return payment.PaymentIntentResult{ProviderRef: in.ProviderRef, Status: payment.StatusCaptured}, nil
}
func (p *paymentTestPort) Cancel(context.Context, payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{}, errors.New("unused test operation")
}
func (p *paymentTestPort) Refund(ctx context.Context, in payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	if err := ctx.Err(); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if err := in.Validate(); err != nil {
		return payment.PaymentIntentResult{}, err
	}
	if in.ProviderRef != "fixture-captured" || in.TenantID != p.captured.TenantID || in.OrderID != p.captured.OrderID || in.OrderVersion != p.captured.OrderVersion || in.Amount.Currency != p.captured.Amount.Currency || in.Amount.Amount > p.captured.Amount.Amount {
		return payment.PaymentIntentResult{}, errors.New("captured payment mismatch")
	}
	if prior, exists := p.requests[in.IdempotencyKey]; exists {
		if prior != in {
			return payment.PaymentIntentResult{}, payment.ErrKeyReused
		}
	} else {
		p.requests[in.IdempotencyKey] = in
		p.events = append(p.events, in)
	}
	return payment.PaymentIntentResult{ProviderRef: in.ProviderRef, Status: payment.StatusPartiallyRefunded}, nil
}
func (p *paymentTestPort) Events() []payment.PaymentOperation {
	return append([]payment.PaymentOperation(nil), p.events...)
}

type testRunner struct {
	commits  int
	active   bool
	snapshot func() func()
}

func (r *testRunner) InTenant(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
	r.active = true
	defer func() { r.active = false }()
	var rollback func()
	if r.snapshot != nil {
		rollback = r.snapshot()
	}
	err := fn(ctx, nil)
	if err == nil {
		r.commits++
	} else if rollback != nil {
		rollback()
	}
	return err
}

type captureReader struct {
	value contract.CapturedPayment
	calls int
	fail  bool
}

func (r *captureReader) CapturedPayment(context.Context, pgx.Tx, string, string) (contract.CapturedPayment, error) {
	r.calls++
	if r.fail {
		return contract.CapturedPayment{}, contract.ErrUnavailable
	}
	return r.value, nil
}

type accountReader struct {
	calls int
	fail  bool
}

func (r *accountReader) RefundAccounts(context.Context, pgx.Tx, string, string) (contract.RefundAccounts, error) {
	r.calls++
	if r.fail {
		return contract.RefundAccounts{}, contract.ErrUnavailable
	}
	return contract.RefundAccounts{Income: account1, Tax: account2, Shipping: account3, Cash: account4}, nil
}

type ledgerFake struct {
	calls  int
	fail   bool
	posted ledger.Transaction
}

func (l *ledgerFake) Post(_ context.Context, _ pgx.Tx, in ledger.Transaction) (string, error) {
	l.calls++
	if l.fail {
		l.fail = false
		return "", errors.New("injected journal failure")
	}
	l.posted = in
	return journalID, nil
}

type refundRepo struct {
	pending   bool
	completed bool
	prepared  contract.PreparedRefund
	prepares  int
	completes int
}

func (r *refundRepo) Request(context.Context, pgx.Tx, string, contract.RequestInput) (contract.Return, error) {
	return contract.Return{}, nil
}
func (r *refundRepo) Receive(context.Context, pgx.Tx, string, contract.ReceiveInput) (contract.Return, error) {
	return contract.Return{}, nil
}
func (r *refundRepo) OrderID(context.Context, pgx.Tx, string, string) (string, error) {
	return orderID, nil
}
func (r *refundRepo) OrderTotal(context.Context, pgx.Tx, string, string) (contract.CapturedPayment, error) {
	return contract.CapturedPayment{TenantID: tenantID, OrderID: orderID, Paid: money.Money{Amount: 270, Currency: "USD"}}, nil
}
func (r *refundRepo) Prepared(_ context.Context, _ pgx.Tx, _ string, in contract.RefundInput) (contract.PreparedRefund, bool, error) {
	if !r.pending {
		return contract.PreparedRefund{}, false, nil
	}
	if r.prepared.RefundKey != in.IdempotencyKey {
		return contract.PreparedRefund{}, false, contract.ErrKeyReused
	}
	out := r.prepared
	if r.completed {
		out.Status = "refunded"
		out.LedgerTransactionID = journalID
	}
	return out, true, nil
}
func (r *refundRepo) Prepare(_ context.Context, _ pgx.Tx, _ string, in contract.RefundInput, capture contract.CapturedPayment, accounts contract.RefundAccounts) (contract.PreparedRefund, error) {
	r.prepares++
	r.pending = true
	r.prepared = contract.PreparedRefund{Return: contract.Return{ID: returnID, OrderID: orderID, OrderVersion: 4, Status: "refund_pending", Kind: contract.UnshippedCancel, RefundMinor: 135}, RefundKey: in.IdempotencyKey, RefundExpectedVersion: in.ExpectedVersion, ProviderRef: capture.ProviderRef, PaymentVersion: capture.OrderVersion, Currency: capture.Paid.Currency, IncomeMinor: 100, TaxMinor: 10, ShippingMinor: 25, IncomeAccountID: accounts.Income, TaxAccountID: accounts.Tax, ShippingAccountID: accounts.Shipping, CashAccountID: accounts.Cash}
	return r.prepared, nil
}
func (r *refundRepo) Complete(_ context.Context, _ pgx.Tx, _ string, in contract.PreparedRefund, journal string) (contract.Return, error) {
	if journal != journalID {
		return contract.Return{}, contract.ErrInvalid
	}
	r.completes++
	r.completed = true
	out := in.Return
	out.Status = "refunded"
	out.OrderVersion++
	r.prepared.Return = out
	r.prepared.LedgerTransactionID = journal
	return out, nil
}

func TestRefundCompletionFailureAndCompletedReplay(t *testing.T) {
	s, r, p, _, _, l, runner := refundFixture(t)
	runner.snapshot = func() func() {
		pending, completed, prepared, posted := r.pending, r.completed, r.prepared, l.posted
		return func() { r.pending, r.completed, r.prepared, l.posted = pending, completed, prepared, posted }
	}
	completeErr := errors.New("injected completion persistence failure")
	calls := 0
	completion := func(_ context.Context, _ pgx.Tx, out contract.Return) error {
		calls++
		if !runner.active || out.Status != "refunded" {
			t.Fatal("completion must see final refund inside its transaction")
		}
		return completeErr
	}
	in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}
	if _, err := s.RefundWithCompletion(context.Background(), tenantID, in, completion); !errors.Is(err, completeErr) {
		t.Fatalf("completion failure was not propagated: %v", err)
	}
	if calls != 1 || r.completed || !r.pending || len(l.posted.Entries) != 0 || runner.commits != 1 || len(p.Events()) != 3 {
		t.Fatalf("completion failure lost pending intent or committed journal: calls=%d pending=%v completed=%v journal=%+v commits=%d events=%d", calls, r.pending, r.completed, l.posted, runner.commits, len(p.Events()))
	}
	completion = func(_ context.Context, _ pgx.Tx, out contract.Return) error {
		calls++
		if !runner.active || out.Status != "refunded" {
			t.Fatal("completion must run before commit")
		}
		return nil
	}
	if _, err := s.RefundWithCompletion(context.Background(), tenantID, in, completion); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefundWithCompletion(context.Background(), tenantID, in, completion); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || !r.completed || len(p.Events()) != 3 || l.calls != 2 {
		t.Fatalf("retry/replay omitted completion or repeated money effects: calls=%d completed=%v events=%d journalcalls=%d", calls, r.completed, len(p.Events()), l.calls)
	}
}

type concurrentlyCompletedRepo struct {
	*refundRepo
	reads int
}

func (r *concurrentlyCompletedRepo) Prepared(ctx context.Context, tx pgx.Tx, tenant string, in contract.RefundInput) (contract.PreparedRefund, bool, error) {
	r.reads++
	if r.reads == 2 {
		r.completed = true
	}
	return r.refundRepo.Prepared(ctx, tx, tenant, in)
}

func TestRefundCompletionConcurrentReceiptReplay(t *testing.T) {
	s, r, p, _, _, l, runner := refundFixture(t)
	s.repo = &concurrentlyCompletedRepo{refundRepo: r}
	calls := 0
	_, err := s.RefundWithCompletion(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}, func(_ context.Context, _ pgx.Tx, out contract.Return) error {
		calls++
		if !runner.active || out.Status != "refunded" {
			t.Fatal("concurrent receipt callback ran outside authorized transaction")
		}
		return nil
	})
	if err != nil || calls != 1 || l.calls != 0 || len(p.Events()) != 3 {
		t.Fatalf("concurrent completion not replayed safely: %v calls=%d journal=%d events=%d", err, calls, l.calls, len(p.Events()))
	}
}
func refundFixture(t *testing.T) (*Service, *refundRepo, *paymentTestPort, *captureReader, *accountReader, *ledgerFake, *testRunner) {
	t.Helper()
	ctx := context.Background()
	p := newPaymentTestPort()
	create := payment.CreatePaymentIntent{TenantID: tenantID, OrderID: orderID, OrderVersion: 1, IdempotencyKey: "create", Amount: money.Money{Amount: 270, Currency: "USD"}}
	intent, err := p.CreateIntent(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	create.IdempotencyKey = "capture"
	if _, err = p.Capture(ctx, payment.PaymentOperation{CreatePaymentIntent: create, ProviderRef: intent.ProviderRef}); err != nil {
		t.Fatal(err)
	}
	c := &captureReader{value: contract.CapturedPayment{TenantID: tenantID, OrderID: orderID, ProviderRef: intent.ProviderRef, OrderVersion: 1, Paid: create.Amount}}
	a := &accountReader{}
	l := &ledgerFake{}
	r := &refundRepo{}
	runner := &testRunner{}
	s := &Service{config: contract.Config{Runner: runner, Payments: p, Captures: c, Accounts: a, Ledger: l}, repo: r}
	return s, r, p, c, a, l, runner
}
func TestRefundRecoversProviderSuccessBeforeDatabaseFailure(t *testing.T) {
	s, r, p, c, a, l, runner := refundFixture(t)
	l.fail = true
	in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund-once"}
	if _, err := s.Refund(context.Background(), tenantID, in); err == nil {
		t.Fatal("injected ledger failure must surface")
	}
	if !r.pending || r.completed || runner.commits != 1 || len(p.Events()) != 3 {
		t.Fatalf("missing durable prepare/provider effect: %+v commits=%d events=%d", r, runner.commits, len(p.Events()))
	}
	c.fail = true
	a.fail = true
	out, err := s.Refund(context.Background(), tenantID, in)
	if err != nil || out.Status != "refunded" || len(p.Events()) != 3 || r.prepares != 1 || c.calls != 1 || a.calls != 1 {
		t.Fatalf("recovery changed immutable PSP request/readers: %+v %v events=%d prepares=%d capture=%d accounts=%d", out, err, len(p.Events()), r.prepares, c.calls, a.calls)
	}
	if err = l.posted.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(l.posted.Entries) != 4 || l.posted.Entries[3].Amount.Amount != 135 || l.posted.SourceType != "return" || l.posted.SourceID != returnID || l.posted.Event != "refund" {
		t.Fatalf("refund journal: %+v", l.posted)
	}
	if _, err = s.Refund(context.Background(), tenantID, in); err != nil || l.calls != 2 || len(p.Events()) != 3 {
		t.Fatalf("completed replay had effect: %v ledger=%d events=%d", err, l.calls, len(p.Events()))
	}
}
func TestRefundRejectsUntrustedCapturedProofBeforeProvider(t *testing.T) {
	for _, mutate := range []func(*contract.CapturedPayment){func(p *contract.CapturedPayment) { p.TenantID = orderID }, func(p *contract.CapturedPayment) { p.OrderID = returnID }, func(p *contract.CapturedPayment) { p.Paid.Amount++ }, func(p *contract.CapturedPayment) { p.Paid.Currency = "EUR" }, func(p *contract.CapturedPayment) { p.OrderVersion = 0 }, func(p *contract.CapturedPayment) { p.ProviderRef = "" }, func(p *contract.CapturedPayment) { p.ProviderRef = strings.Repeat("x", 257) }} {
		s, r, p, c, _, l, _ := refundFixture(t)
		mutate(&c.value)
		if _, err := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}); !errors.Is(err, contract.ErrUnavailable) {
			t.Fatalf("invalid captured proof accepted: %v", err)
		}
		if r.prepares != 0 || l.calls != 0 || len(p.Events()) != 2 {
			t.Fatal("invalid proof reached a side effect")
		}
	}
}

type incorrectRefundProvider struct {
	payment.Port
	result payment.PaymentIntentResult
}

func (p incorrectRefundProvider) Refund(context.Context, payment.PaymentOperation) (payment.PaymentIntentResult, error) {
	return p.result, nil
}
func TestRefundRejectsUnconfirmedProviderResult(t *testing.T) {
	for _, status := range []payment.PaymentStatus{payment.StatusPending, payment.StatusAuthorized, payment.StatusCaptured, payment.StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			s, r, p, c, _, l, _ := refundFixture(t)
			s.config.Payments = incorrectRefundProvider{Port: p, result: payment.PaymentIntentResult{ProviderRef: c.value.ProviderRef, Status: status}}
			if _, err := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}); err == nil {
				t.Fatal("non-final refund result accepted")
			}
			if l.calls != 0 || r.completed {
				t.Fatal("unconfirmed refund reached ledger/completion")
			}
		})
	}
	s, r, p, _, _, l, _ := refundFixture(t)
	s.config.Payments = incorrectRefundProvider{Port: p, result: payment.PaymentIntentResult{ProviderRef: "different", Status: payment.StatusRefunded}}
	if _, err := s.Refund(context.Background(), tenantID, contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}); err == nil || l.calls != 0 || r.completed {
		t.Fatal("provider-reference mismatch accepted")
	}
}
func TestRefundRejectsMalformedDurableIntent(t *testing.T) {
	for name, mutate := range map[string]func(*contract.PreparedRefund){"negative component": func(p *contract.PreparedRefund) { p.TaxMinor = -1 }, "unbalanced": func(p *contract.PreparedRefund) { p.RefundMinor++ }, "overflow": func(p *contract.PreparedRefund) { p.IncomeMinor = 1<<63 - 1; p.TaxMinor = 1 }, "invalid account": func(p *contract.PreparedRefund) { p.CashAccountID = "bad" }, "changed version": func(p *contract.PreparedRefund) { p.RefundExpectedVersion++ }, "changed currency": func(p *contract.PreparedRefund) { p.Currency = "usd" }} {
		t.Run(name, func(t *testing.T) {
			s, r, p, c, a, l, _ := refundFixture(t)
			in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}
			_, _ = r.Prepare(context.Background(), nil, tenantID, in, c.value, contract.RefundAccounts{Income: account1, Tax: account2, Shipping: account3, Cash: account4})
			mutate(&r.prepared)
			if _, err := s.Refund(context.Background(), tenantID, in); err == nil {
				t.Fatal("malformed prepared intent accepted")
			}
			if len(p.Events()) != 2 || l.calls != 0 || c.calls != 0 || a.calls != 0 {
				t.Fatal("malformed durable intent reached mutable readers/provider/ledger")
			}
		})
	}
}
func TestRefundCompletedReplaySurvivesMissingProviderConfiguration(t *testing.T) {
	s, r, p, c, a, l, _ := refundFixture(t)
	in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}
	if _, err := s.Refund(context.Background(), tenantID, in); err != nil {
		t.Fatal(err)
	}
	s.config.Payments = nil
	s.config.Ledger = nil
	s.config.Captures = nil
	s.config.Accounts = nil
	if _, err := s.Refund(context.Background(), tenantID, in); err != nil || !r.completed || l.calls != 1 || len(p.Events()) != 3 || c.calls != 1 || a.calls != 1 {
		t.Fatalf("completed durable replay depended on mutable binding: %v", err)
	}
}
func TestRefundJournalZeroComponentsRemainBalanced(t *testing.T) {
	s, r, _, c, _, _, _ := refundFixture(t)
	_ = s
	in := contract.RefundInput{ReturnID: returnID, ExpectedVersion: 4, IdempotencyKey: "refund"}
	p, err := r.Prepare(context.Background(), nil, tenantID, in, c.value, contract.RefundAccounts{Income: account1, Tax: account2, Shipping: account3, Cash: account4})
	if err != nil {
		t.Fatal(err)
	}
	p.TaxMinor = 0
	p.ShippingMinor = 0
	p.RefundMinor = p.IncomeMinor
	journal, err := refundJournal(tenantID, p)
	if err != nil || len(journal.Entries) != 2 || journal.Validate() != nil {
		t.Fatalf("zero component journal %+v %v", journal, err)
	}
}
