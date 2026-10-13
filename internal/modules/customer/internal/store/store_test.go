// Responsibility: public repository error/custody boundary tests, not SQL proof.
// Dependencies: generated db query port and in-memory pgx rows; GitHub CI runs it.
// Invariants: backend failures disclose no secrets, foreign/malformed documents deny.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/internal/store"
)

const storeTenant = "11111111-1111-4111-8111-111111111111"

type documentDB struct{ document string }

func (d documentDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected execution")
}
func (d documentDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (d documentDB) QueryRow(context.Context, string, ...any) pgx.Row { return documentRow(d.document) }

type documentRow string

func (r documentRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected row shape")
	}
	text, ok := dest[0].(*string)
	if !ok {
		return errors.New("unexpected document type")
	}
	*text = string(r)
	return nil
}

func documentRepository(document string) c.Repository {
	return store.New(func(ctx context.Context, tenant string, fn func(*db.Queries) error) error {
		return fn(db.New(documentDB{document: document}))
	})
}

func TestCustomerRepositoryFailClosedAndRedacted(t *testing.T) {
	ctx := context.Background()
	if _, err := store.New(nil).Credential(ctx, storeTenant, "buyer@example.test"); !errors.Is(err, c.ErrUnavailable) {
		t.Fatal("missing runner must fail closed")
	}
	called := false
	repo := store.New(func(context.Context, string, func(*db.Queries) error) error {
		called = true
		return errors.New("backend disclosed connection and private credential")
	})
	if _, err := repo.Credential(ctx, "foreign-tenant", "buyer@example.test"); !errors.Is(err, c.ErrUnavailable) || called {
		t.Fatal("invalid tenant entered persistence")
	}
	credential, err := repo.Credential(ctx, storeTenant, "buyer@example.test")
	if !errors.Is(err, c.ErrUnavailable) || strings.Contains(err.Error(), "private") || credential.Hash != "" || credential.CustomerID != "" {
		t.Fatal("backend failure must return a generic error and empty credential")
	}
	called = false
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.Credential(canceled, storeTenant, "buyer@example.test"); !errors.Is(err, context.Canceled) || called {
		t.Fatal("canceled request entered persistence")
	}
}

func TestCustomerRepositorySessionDocumentCustody(t *testing.T) {
	fixture := c.Session{ID: "22222222-2222-4222-8222-222222222222", TenantID: storeTenant, CustomerID: "33333333-3333-4333-8333-333333333333", Email: "buyer@example.test", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), IdleExpiresAt: time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal("fixture JSON unavailable")
	}
	session, err := documentRepository(string(encoded)).Session(context.Background(), storeTenant, [32]byte{}, time.Hour)
	if err != nil || session.TenantID != storeTenant || session.CustomerID != fixture.CustomerID || session.Token != "" {
		t.Fatal("valid metadata-only session must preserve tenant custody")
	}
	if _, err := documentRepository("").Session(context.Background(), storeTenant, [32]byte{}, time.Hour); !errors.Is(err, c.ErrUnauthorized) {
		t.Fatal("missing session must deny authorization")
	}
	for _, document := range []string{
		"null", "{bad}", string(encoded) + "{}", strings.Repeat("x", 8193),
		strings.Replace(string(encoded), storeTenant, "99999999-9999-4999-8999-999999999999", 1),
		strings.Replace(string(encoded), `"session_id":"`+fixture.ID+`"`, `"session_id":"bad"`, 1),
		strings.TrimSuffix(string(encoded), "}") + `,"token":"private-challenge"}`,
	} {
		session, err := documentRepository(document).Session(context.Background(), storeTenant, [32]byte{}, time.Hour)
		if !errors.Is(err, c.ErrUnavailable) || strings.Contains(err.Error(), "private-challenge") || session.Token != "" || session.CustomerID != "" {
			t.Fatal("foreign, malformed or secret-bearing document must be rejected")
		}
	}
}
