// Responsibility: pin the runtime guest-cart snapshot role boundary.
// Dependencies: checkout actor and generated cart SQL; caller: bootstrap tests.
// Invariant: a login-role snapshot must switch to app_runtime before cart RLS reads.
package bootstrap

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	payment "github.com/luogangan7-lgtm/opensite-commerce/internal/contracts/payment/v1"
	checkoutcontract "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	httpcheckout "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
)

type guestCartSnapshotRow struct{ values []any }

func (r guestCartSnapshotRow) Scan(dest ...any) error {
	if len(dest) != len(r.values) {
		return errors.New("unexpected cart row shape")
	}
	for i, value := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

type guestCartSnapshotRows struct{ pgx.Rows }

func (*guestCartSnapshotRows) Next() bool { return false }
func (*guestCartSnapshotRows) Err() error { return nil }
func (*guestCartSnapshotRows) Close()     {}

type guestCartSnapshotTx struct {
	pgx.Tx
	tenant, cartID, role string
	hash                 []byte
	failRole             bool
	roleSet, cartRead    bool
}

func (tx *guestCartSnapshotTx) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(query, "name: SetCartRuntimeRole"):
		if tx.failRole {
			return pgconn.CommandTag{}, errors.New("role switch refused")
		}
		tx.role = "app_runtime"
		tx.roleSet = true
	case !strings.Contains(query, "name: SetCartContext"):
		return pgconn.CommandTag{}, errors.New("unexpected snapshot write")
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (tx *guestCartSnapshotTx) QueryRow(_ context.Context, query string, _ ...any) pgx.Row {
	switch {
	case strings.Contains(query, "name: GetCartContext"):
		return guestCartSnapshotRow{[]any{tx.tenant, "", "", tx.role}}
	case strings.Contains(query, "name: GetCart"):
		tx.cartRead = true
		return guestCartSnapshotRow{[]any{
			pgtype.UUID{Bytes: uuid.MustParse(tx.tenant), Valid: true},
			pgtype.UUID{Bytes: uuid.MustParse(tx.cartID), Valid: true},
			pgtype.UUID{}, tx.hash, "USD", int64(1), pgtype.Timestamptz{},
		}}
	default:
		panic("unexpected guest cart snapshot query")
	}
}

func (*guestCartSnapshotTx) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(query, "name: ListCartLines") {
		return nil, errors.New("unexpected snapshot query")
	}
	return &guestCartSnapshotRows{}, nil
}

func TestGuestCartSnapshotSwitchesToRuntimeRoleBeforeRead(t *testing.T) {
	const tenant = "c0900001-0000-4000-8000-000000000001"
	const cartID = "c2400001-0000-4000-8000-000000000001"
	hash := make([]byte, 32)
	actor, err := checkoutcontract.NewAuthorizedActor(tenant, "guest", hex.EncodeToString(hash))
	if err != nil {
		t.Fatal(err)
	}
	for _, failRole := range []bool{false, true} {
		tx := &guestCartSnapshotTx{tenant: tenant, cartID: cartID, role: "opensite_runtime_login", hash: hash, failRole: failRole}
		err := verifyGuestCartSnapshot(context.Background(), tx, actor, cartID)
		if failRole {
			if err == nil || tx.cartRead {
				t.Fatalf("failed role switch reached cart: err=%v read=%v", err, tx.cartRead)
			}
		} else if err != nil || !tx.roleSet || !tx.cartRead {
			t.Fatalf("valid runtime snapshot rejected: err=%v role=%v read=%v", err, tx.roleSet, tx.cartRead)
		}
	}
}

type checkoutRoleRunner struct {
	guestCartSnapshotTx
	commands, snapshots, capabilities int
}

func (r *checkoutRoleRunner) RunAuthorizedTx(_ context.Context, _ string, _ func(context.Context, pgx.Tx) error) error {
	r.commands++
	return errors.New("authorized tenant stamp required")
}
func (r *checkoutRoleRunner) RunAuthorizedTxSnapshot(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
	r.snapshots++
	return fn(ctx, &r.guestCartSnapshotTx)
}
func (r *checkoutRoleRunner) RunCapabilityTx(ctx context.Context, _ string, fn func(context.Context, pgx.Tx) error) error {
	r.capabilities++
	return fn(ctx, &r.guestCartSnapshotTx)
}

type fakePaymentContextService struct{ ctx context.Context }

func (*fakePaymentContextService) Start(context.Context, checkoutcontract.AuthorizedActor, checkoutcontract.StartInput) (checkoutcontract.Result, error) {
	return checkoutcontract.Result{}, errors.New("unexpected checkout start")
}
func (s *fakePaymentContextService) Confirm(ctx context.Context, _ payment.VerifiedEvent) (checkoutcontract.Result, error) {
	s.ctx = ctx
	return checkoutcontract.Result{CheckoutID: "c2400002-0000-4000-8000-000000000001", OrderID: "c2400003-0000-4000-8000-000000000001", State: "paid", AmountMinor: 1200, Currency: "USD", OrderVersion: 1, Payment: checkoutcontract.Intent{ProviderRef: "fake-test", Status: payment.StatusCaptured}}, nil
}
func (*fakePaymentContextService) Cancel(context.Context, checkoutcontract.AuthorizedActor, checkoutcontract.CancelInput) (checkoutcontract.Result, error) {
	return checkoutcontract.Result{}, errors.New("unexpected checkout cancel")
}

func TestCheckoutRoleRunnerRequiresSignedFakeProof(t *testing.T) {
	const tenant = "c0900001-0000-4000-8000-000000000001"
	const other = "c0900001-0000-4000-8000-000000000002"
	const event = `{"event_id":"fake-proof","tenant_id":"` + tenant + `","order_id":"c2400003-0000-4000-8000-000000000001","order_version":1,"provider_ref":"fake-test","status":"captured","amount_minor":1200,"currency":"USD"}`
	key := make([]byte, 32)
	verifier, err := httpcheckout.NewFakeHMACVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	service := &fakePaymentContextService{}
	handler, err := httpcheckout.New(httpcheckout.Config{Service: service, FakeEnabled: true, Environment: "test", Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.New(handler.Authenticator(nil), nil)
	for _, route := range handler.Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	request := func(signature string) int {
		req := httptest.NewRequest(http.MethodPost, httpcheckout.FakePattern, strings.NewReader(event))
		req.Header.Set("Content-Type", "application/json")
		if signature != "" {
			req.Header.Set("X-Fake-Signature", signature)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res.Code
	}
	runner := &checkoutRoleRunner{guestCartSnapshotTx: guestCartSnapshotTx{role: "opensite_runtime_login"}}
	if got := request(""); got != 401 || service.ctx != nil {
		t.Fatalf("unsigned Fake request admitted: status=%d context=%v", got, service.ctx)
	}
	if err := checkoutCommandRun(runner)(context.Background(), tenant, func(context.Context, pgx.Tx) error { return nil }); err == nil || runner.capabilities != 0 || runner.commands != 1 {
		t.Fatalf("unsigned context reached capability transaction: err=%v commands=%d capabilities=%d", err, runner.commands, runner.capabilities)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(event))
	if got := request(hex.EncodeToString(mac.Sum(nil))); got != 200 || service.ctx == nil {
		t.Fatalf("signed Fake request rejected: status=%d context=%v", got, service.ctx)
	}
	if err := checkoutCommandRun(runner)(service.ctx, other, func(context.Context, pgx.Tx) error { return nil }); !errors.Is(err, checkoutcontract.ErrForbidden) || runner.capabilities != 0 {
		t.Fatalf("verified tenant mismatch entered transaction: err=%v capabilities=%d", err, runner.capabilities)
	}
	called := false
	if err := checkoutCommandRun(runner)(service.ctx, tenant, func(context.Context, pgx.Tx) error { called = true; return nil }); err != nil || !called || !runner.roleSet || runner.capabilities != 1 {
		t.Fatalf("verified Fake transaction failed role admission: err=%v called=%v role=%v capabilities=%d", err, called, runner.roleSet, runner.capabilities)
	}
	runner.roleSet = false
	called = false
	if err := checkoutSnapshotRun(runner)(context.Background(), tenant, func(context.Context, pgx.Tx) error { called = true; return nil }); err != nil || !called || !runner.roleSet || runner.snapshots != 1 {
		t.Fatalf("preview snapshot failed role admission: err=%v called=%v role=%v snapshots=%d", err, called, runner.roleSet, runner.snapshots)
	}
}
