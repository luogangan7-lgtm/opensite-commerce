//go:build pgx

// Responsibility: exercise customer public auth ports against migrated CI PostgreSQL.
// Dependencies: customer module, capability-checked customer runner and owner fixture runner.
// Invariants: runtime credentials, isolated expendable tenants, mailbox proof before authority,
// hash-only token custody, and no raw PostgreSQL connection outside pgxadapter.
package customer_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	customer "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

const noPreviousCustomerToken = ""

type customerPGMessage struct{ tenant, email, purpose, token string }
type customerPGMailbox struct {
	mu       sync.Mutex
	messages []customerPGMessage
}

func (m *customerPGMailbox) Send(_ context.Context, tenant, email, purpose, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, customerPGMessage{tenant, email, purpose, token})
	return nil
}

func (m *customerPGMailbox) latest(t *testing.T, tenant, purpose string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.messages) - 1; i >= 0; i-- {
		if message := m.messages[i]; message.tenant == tenant && message.purpose == purpose {
			return message.token
		}
	}
	t.Fatal("CUSTOMER_PG_MAILBOX_MISSING")
	return ""
}

type customerPGFixture struct {
	ctx                  context.Context
	owner                pgxadapter.Runner
	runtime              *pgxadapter.CustomerRunner
	service              c.Service
	mailbox              *customerPGMailbox
	tenant, other, email string
	ip                   netip.Addr
}

func customerPGBytes(t *testing.T, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("CUSTOMER_PG_RANDOM_FAILED")
	}
	return b
}

func customerPGCredential(t *testing.T) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(customerPGBytes(t, 32))
}

func customerPG(t *testing.T) customerPGFixture {
	t.Helper()
	ownerDSN, customerDSN := os.Getenv("DATABASE_URL"), os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL")
	if ownerDSN == "" || customerDSN == "" {
		if os.Getenv("CUSTOMER_AUTH_REQUIRE_PG") == "1" {
			t.Fatal("NOT_RUN: CUSTOMER_AUTH_PG_DSN_MISSING")
		}
		t.Skip("NOT_RUN: customer PostgreSQL lifecycle requires owner and dedicated capability DSNs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	owner, closeOwner, err := pgxadapter.Open(ctx, ownerDSN)
	if err != nil {
		t.Fatal("CUSTOMER_PG_OWNER_CONNECT_FAILED")
	}
	t.Cleanup(closeOwner)
	unsafe, capabilityErr := pgxadapter.OpenCustomerRunner(ctx, ownerDSN)
	if unsafe != nil {
		unsafe.Close()
	}
	if capabilityErr == nil {
		t.Fatal("CUSTOMER_PG_OWNER_CAPABILITY_ACCEPTED")
	}
	runtime, err := pgxadapter.OpenCustomerRunner(ctx, customerDSN)
	if err != nil {
		t.Fatal("CUSTOMER_PG_CAPABILITY_CONNECT_FAILED")
	}
	t.Cleanup(runtime.Close)
	mailbox := &customerPGMailbox{}
	service, err := customer.NewPostgres(runtime.InTenant, customer.Config{Mailer: mailbox, Secret: customerPGBytes(t, 32)})
	if err != nil {
		t.Fatal("CUSTOMER_PG_SERVICE_CONFIG_FAILED")
	}
	f := customerPGFixture{ctx: ctx, owner: owner, runtime: runtime, service: service, mailbox: mailbox, tenant: uuid.NewString(), other: uuid.NewString(), email: uuid.NewString() + "@example.invalid", ip: netip.MustParseAddr("192.0.2.81")}
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO app.tenants(id,name) VALUES($1,'customer-go-gate'),($2,'customer-go-gate-other')`, f.tenant, f.other)
		return err
	})
	// The CI database is expendable; random tenant IDs avoid interference with
	// other gates, and database teardown owns sealed-order fixture destruction.
	return f
}

func (f customerPGFixture) fixture(t *testing.T, fn func(context.Context, pgx.Tx) error) {
	t.Helper()
	if err := f.owner.InMaintenance(f.ctx, fn); err != nil {
		t.Fatal("CUSTOMER_PG_FIXTURE_FAILED")
	}
}

func customerPGTokenHash(t *testing.T, token string) [32]byte {
	t.Helper()
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("CUSTOMER_PG_TOKEN_SHAPE_INVALID")
	}
	return sha256.Sum256(decoded)
}

func (f customerPGFixture) expireChallenge(t *testing.T, tenant, token string) {
	t.Helper()
	hash := customerPGTokenHash(t, token)
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.customer_auth_tokens SET created_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1 AND token_hash=$2 AND consumed_at IS NULL`, tenant, hash[:])
		return err
	})
}

func (f customerPGFixture) requireCurrent(t *testing.T, tenant, token string, allowed bool) c.Session {
	t.Helper()
	session, err := f.service.Current(f.ctx, tenant, token)
	if allowed {
		if err != nil || session.CustomerID == "" || session.TenantID != tenant || session.Token != "" {
			t.Fatal("CUSTOMER_PG_CURRENT_MISSING")
		}
	} else if !errors.Is(err, c.ErrUnauthorized) {
		t.Fatal("CUSTOMER_PG_SESSION_AUTHORITY_LEAK")
	}
	return session
}

func TestCustomerPGMailboxLifecycleTenantRotationAndReset(t *testing.T) {
	f := customerPG(t)
	password := customerPGCredential(t)
	otherPassword := customerPGCredential(t)
	newPassword := customerPGCredential(t)
	guestID, orderID := uuid.NewString(), uuid.NewString()
	var originalSnapshot string
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, f.tenant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,$3)`, f.tenant, guestID, f.email); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_orders_executor`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app.orders(tenant_id,id,order_number,creation_key,request_hash,guest_contact,currency,items_net_minor,tax_minor,shipping_minor,shipping_snapshot,total_minor) VALUES($1,$2::text::uuid,1,$2::text,sha256($2::text::bytea),jsonb_build_object('email',$3::text,'name','Original'),'USD',0,0,0,'{}',0)`, f.tenant, orderID, strings.ToUpper(f.email)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE app.orders SET sealed=true WHERE tenant_id=$1 AND id=$2`, f.tenant, orderID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM app.orders o WHERE tenant_id=$1 AND id=$2`, f.tenant, orderID).Scan(&originalSnapshot)
	})
	err := f.service.Register(f.ctx, f.tenant, f.email, password, f.ip)
	if err != nil {
		t.Fatal("CUSTOMER_PG_REGISTER_FAILED")
	}
	verify := f.mailbox.latest(t, f.tenant, "register")
	verifyHash := customerPGTokenHash(t, verify)
	var credentials, links, hashes int
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.customer_credentials WHERE tenant_id=$1 AND customer_id=$2`, f.tenant, guestID).Scan(&credentials); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.customer_guest_order_links WHERE tenant_id=$1`, f.tenant).Scan(&links); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id=$1 AND token_hash=$2 AND octet_length(token_hash)=32`, f.tenant, verifyHash[:]).Scan(&hashes)
	})
	if credentials != 0 || links != 0 || hashes != 1 {
		t.Fatal("CUSTOMER_PG_PROOF_BEFORE_AUTHORITY_BROKEN")
	}
	_, err = f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, noPreviousCustomerToken)
	if !errors.Is(err, c.ErrInvalidCredentials) {
		t.Fatal("CUSTOMER_PG_UNVERIFIED_LOGIN_ACCEPTED")
	}
	if err = f.service.Verify(f.ctx, f.other, verify); !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_VERIFY_CROSSED_TENANT")
	}
	if err = f.service.Verify(f.ctx, f.tenant, verify); err != nil {
		t.Fatal("CUSTOMER_PG_VERIFY_FAILED")
	}
	if err = f.service.Verify(f.ctx, f.tenant, verify); !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_VERIFY_REPLAY_ACCEPTED")
	}
	// A generated command that succeeds inside a callback must still roll back
	// when the caller rejects the transaction; later requests get clean scope.
	rollbackHash := sha256.Sum256(customerPGBytes(t, 32))
	rollbackMarker := errors.New("customer fixture rollback")
	err = f.runtime.InTenant(f.ctx, f.tenant, func(q *db.Queries) error {
		prepared, err := q.PrepareCustomerReset(f.ctx, db.PrepareCustomerResetParams{Email: f.email, TokenHash: rollbackHash[:], ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}})
		if err != nil {
			return err
		}
		if !prepared {
			return errors.New("customer fixture reset not prepared")
		}
		return rollbackMarker
	})
	if !errors.Is(err, rollbackMarker) {
		t.Fatal("CUSTOMER_PG_CALLBACK_ERROR_NOT_PRESERVED")
	}
	var rollbackCount int
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM app.customer_auth_tokens WHERE tenant_id=$1 AND token_hash=$2`, f.tenant, rollbackHash[:]).Scan(&rollbackCount)
	})
	if rollbackCount != 0 {
		t.Fatal("CUSTOMER_PG_CALLBACK_ROLLBACK_FAILED")
	}
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	called := false
	err = f.runtime.InTenant(canceled, f.tenant, func(*db.Queries) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal("CUSTOMER_PG_CANCELED_CALLBACK_EXECUTED")
	}
	var snapshot string
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app.customer_guest_order_links WHERE tenant_id=$1 AND customer_id=$2 AND order_id=$3`, f.tenant, guestID, orderID).Scan(&links); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM app.orders o WHERE tenant_id=$1 AND id=$2`, f.tenant, orderID).Scan(&snapshot)
	})
	if links != 1 || snapshot != originalSnapshot {
		t.Fatal("CUSTOMER_PG_GUEST_PROOF_CHANGED_ORDER_SNAPSHOT")
	}
	first, err := f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, noPreviousCustomerToken)
	if err != nil || first.CustomerID != guestID || first.Token == "" {
		t.Fatal("CUSTOMER_PG_LOGIN_FAILED")
	}
	f.requireCurrent(t, f.other, first.Token, false)
	f.requireCurrent(t, f.tenant, first.Token, true)
	rotated, err := f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, first.Token)
	if err != nil || rotated.ID == first.ID || rotated.Token == first.Token {
		t.Fatal("CUSTOMER_PG_SESSION_ROTATION_FAILED")
	}
	f.requireCurrent(t, f.tenant, first.Token, false)
	f.requireCurrent(t, f.tenant, rotated.Token, true)
	_, err = f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, first.Token)
	if !errors.Is(err, c.ErrUnauthorized) {
		t.Fatal("CUSTOMER_PG_ROTATION_REPLAY_ACCEPTED")
	}
	err = f.service.Register(f.ctx, f.other, f.email, otherPassword, f.ip)
	if err != nil {
		t.Fatal("CUSTOMER_PG_SECOND_TENANT_REGISTER_FAILED")
	}
	if err = f.service.Verify(f.ctx, f.other, f.mailbox.latest(t, f.other, "register")); err != nil {
		t.Fatal("CUSTOMER_PG_SECOND_TENANT_VERIFY_FAILED")
	}
	otherSession, err := f.service.Login(f.ctx, f.other, f.email, otherPassword, f.ip, noPreviousCustomerToken)
	if err != nil || otherSession.CustomerID == guestID {
		t.Fatal("CUSTOMER_PG_SAME_EMAIL_TENANT_IDENTITY_SHARED")
	}
	if err = f.service.RequestReset(f.ctx, f.tenant, f.email, f.ip); err != nil {
		t.Fatal("CUSTOMER_PG_RESET_REQUEST_FAILED")
	}
	reset := f.mailbox.latest(t, f.tenant, "reset")
	if err = f.service.Verify(f.ctx, f.tenant, reset); !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_CHALLENGE_PURPOSE_BYPASS")
	}
	err = f.service.Reset(f.ctx, f.other, reset, newPassword, f.ip)
	if !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_RESET_CROSSED_TENANT")
	}
	err = f.service.Reset(f.ctx, f.tenant, reset, newPassword, f.ip)
	if err != nil {
		t.Fatal("CUSTOMER_PG_RESET_FAILED")
	}
	err = f.service.Reset(f.ctx, f.tenant, reset, newPassword, f.ip)
	if !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_RESET_REPLAY_ACCEPTED")
	}
	f.requireCurrent(t, f.tenant, rotated.Token, false)
	f.requireCurrent(t, f.other, otherSession.Token, true)
	_, err = f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, noPreviousCustomerToken)
	if !errors.Is(err, c.ErrInvalidCredentials) {
		t.Fatal("CUSTOMER_PG_OLD_CREDENTIAL_SURVIVED_RESET")
	}
	current, err := f.service.Login(f.ctx, f.tenant, f.email, newPassword, f.ip, noPreviousCustomerToken)
	if err != nil {
		t.Fatal("CUSTOMER_PG_NEW_CREDENTIAL_LOGIN_FAILED")
	}
	if err = f.service.Logout(f.ctx, f.tenant, current.Token); err != nil {
		t.Fatal("CUSTOMER_PG_LOGOUT_FAILED")
	}
	f.requireCurrent(t, f.tenant, current.Token, false)
}

func TestCustomerPGChallengeExpiryConcurrentConsumeAndSessionExpiry(t *testing.T) {
	f := customerPG(t)
	password := customerPGCredential(t)
	err := f.service.Register(f.ctx, f.tenant, f.email, password, f.ip)
	if err != nil {
		t.Fatal("CUSTOMER_PG_REGISTER_FAILED")
	}
	token := f.mailbox.latest(t, f.tenant, "register")
	f.expireChallenge(t, f.tenant, token)
	if err = f.service.Verify(f.ctx, f.tenant, token); !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_EXPIRED_VERIFY_ACCEPTED")
	}
	err = f.service.Register(f.ctx, f.tenant, f.email, password, f.ip)
	if err != nil {
		t.Fatal("CUSTOMER_PG_REGISTER_RETRY_FAILED")
	}
	token = f.mailbox.latest(t, f.tenant, "register")
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- f.service.Verify(f.ctx, f.tenant, token) }()
	}
	success, rejected := 0, 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result == nil {
			success++
		} else if errors.Is(result, c.ErrToken) {
			rejected++
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatal("CUSTOMER_PG_CONCURRENT_VERIFY_NOT_SINGLE_USE")
	}
	session, err := f.service.Login(f.ctx, f.tenant, f.email, password, f.ip, noPreviousCustomerToken)
	if err != nil {
		t.Fatal("CUSTOMER_PG_LOGIN_FAILED")
	}
	if err = f.service.RequestReset(f.ctx, f.tenant, f.email, f.ip); err != nil {
		t.Fatal("CUSTOMER_PG_RESET_REQUEST_FAILED")
	}
	reset := f.mailbox.latest(t, f.tenant, "reset")
	f.expireChallenge(t, f.tenant, reset)
	err = f.service.Reset(f.ctx, f.tenant, reset, password, f.ip)
	if !errors.Is(err, c.ErrToken) {
		t.Fatal("CUSTOMER_PG_EXPIRED_RESET_ACCEPTED")
	}
	f.requireCurrent(t, f.tenant, session.Token, true)
	hash := customerPGTokenHash(t, session.Token)
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.customer_sessions SET created_at=clock_timestamp()-interval '2 hours',last_seen_at=clock_timestamp()-interval '2 hours',idle_expires_at=clock_timestamp()-interval '1 hour' WHERE tenant_id=$1 AND token_hash=$2`, f.tenant, hash[:])
		return err
	})
	f.requireCurrent(t, f.tenant, session.Token, false)
}

// Executor authority is part of the caller capability: owning trusted proof
// functions must never grant permission to replace objects in their schema.
func TestCustomerPGRejectsExecutorPrivilegeDrift(t *testing.T) {
	f := customerPG(t)
	// Register cleanup before injection; it must run even when the assertion
	// fails or the request context expires. Other customer PG tests are serial.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.owner.InMaintenance(cleanupCtx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `REVOKE CREATE ON SCHEMA app FROM app_customer_executor`)
			return err
		}); err != nil {
			t.Error("CUSTOMER_PG_EXECUTOR_PRIVILEGE_CLEANUP_FAILED")
		}
	}()
	f.fixture(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `GRANT CREATE ON SCHEMA app TO app_customer_executor`)
		return err
	})
	runner, err := pgxadapter.OpenCustomerRunner(f.ctx, os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL"))
	if runner != nil {
		runner.Close()
	}
	if err == nil {
		t.Fatal("CUSTOMER_PG_EXECUTOR_PRIVILEGE_DRIFT_RED: executor schema CREATE was accepted")
	}
}
