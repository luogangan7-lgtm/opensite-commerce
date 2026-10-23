//go:build pgx

// Responsibility: HTTP + real PostgreSQL regression for B2B runtime wiring.
// Dependencies: production customer sessions, publish public reader and quotes ports;
// CI runs this against disposable databases. Fixture SQL provisions identities only.
// Invariants: verified host owns tenant/site; company roles and accept-before-approve
// remain authoritative; staff cookies and request identity hints grant nothing.
package bootstrap_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	checkouthttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/checkout"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/customerhttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
)

type b2bStaffAdmission struct {
	tenant, staff string
	calls         int
}

func (a *b2bStaffAdmission) Authenticate(ctx context.Context, _ *http.Request, _ httpapi.RouteSpec) (context.Context, error) {
	a.calls++
	return qc.WithTrustedActor(ctx, qc.ActorIdentity{TenantID: a.tenant, StaffID: a.staff}), nil
}

func TestB2BWiringHTTPPG(t *testing.T) {
	f := quoteOrdersDB(t, time.Hour)
	host := "b2b-" + f.site[:8] + ".example.invalid"
	origin := "https://" + host
	purchaser, outsider := uuid.NewString(), uuid.NewString()
	f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
		steps := []struct {
			sql  string
			args []any
		}{
			{`UPDATE app.buyer_company_members SET role_code='purchaser' WHERE tenant_id=$1 AND customer_id=$2`, []any{f.tenant, f.buyer}},
			{`INSERT INTO app.customers(tenant_id,id,email) VALUES($1,$2,$4),($1,$3,$5)`, []any{f.tenant, purchaser, outsider, purchaser + "@example.test", outsider + "@example.test"}},
			{`INSERT INTO app.buyer_company_members(tenant_id,buyer_company_id,customer_id,role_code) VALUES($1,$2,$3,'purchaser'),($1,$4,$5,'approver')`, []any{f.tenant, f.company, purchaser, f.otherCompany, outsider}},
			// A sealed published site and a verified domain use the existing public
			// resolver role, never runtime SQL or client-supplied tenant headers.
			{`SELECT set_config('app.tenant_id',$1,true)`, []any{f.tenant}},
			{`INSERT INTO app.site_releases(tenant_id,site_id,id,release_no,renderer_build_id,manifest,manifest_hash) VALUES($1,$2,$3,1,'b2b-test','{"version":1,"locale":"en","title":"B2B","description":"","navigation":[],"products":[]}',sha256(convert_to('b2b','UTF8')))`, []any{f.tenant, f.site, uuid.NewString()}},
		}
		for _, step := range steps {
			if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
				return err
			}
		}
		var release string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM app.site_releases WHERE tenant_id=$1 AND site_id=$2`, f.tenant, f.site).Scan(&release); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_domain_verifier`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app.site_domains(tenant_id,site_id,hostname,status,verified_at) VALUES($1,$2,$3,'verified',clock_timestamp())`, f.tenant, f.site, host); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_publisher`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT app.publish_release($1,$2,$3,NULL)`, f.tenant, f.site, release)
		return err
	})
	if os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL") == "" {
		pgxtest.Skip(t, "NOT_RUN: customer authentication database required")
	}
	runner, err := pgxadapter.OpenCustomerRunner(f.ctx, os.Getenv("CUSTOMER_AUTH_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	secret, mail := quoteRandom(t), &quoteCustomerMailbox{}
	service, err := customer.NewPostgres(runner.InTenant, customer.Config{Mailer: mail, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	cookies := map[string]*http.Cookie{}
	for _, id := range []string{f.buyer, f.approver, purchaser, outsider} {
		password := base64.RawURLEncoding.EncodeToString(quoteRandom(t))
		ip := netip.MustParseAddr("192.0.2.81")
		if err := service.Register(f.ctx, f.tenant, id+"@example.test", password, ip); err != nil {
			t.Fatal(err)
		}
		if err := service.Verify(f.ctx, f.tenant, mail.token); err != nil {
			t.Fatal(err)
		}
		session, err := service.Login(f.ctx, f.tenant, id+"@example.test", password, ip, "")
		if err != nil {
			t.Fatal(err)
		}
		cookies[id] = &http.Cookie{Name: customerhttp.SessionCookie, Value: session.Token}
	}
	// Exercise the actual environment composition without enabling any unrelated
	// capability. No DSN or token is included in assertions or test output.
	t.Setenv("OSC_PUBLIC_DATABASE_URL", loginDSN(t, f.ctx, f.runner.Runner, os.Getenv("DATABASE_URL"), "b2b_public_", "app_domain_resolver"))
	receiver, _ := f.factReceiver(t)
	staffAdmission := &b2bStaffAdmission{tenant: f.tenant, staff: f.staff}
	config, closeRuntime, err := bootstrap.FromEnvironment(f.ctx, bootstrap.Config{
		Customer: service, CustomerOrigin: origin, CustomerSecret: secret,
		Quotes:          quotes.NewApplication(f.runner),
		Authenticator:   staffAdmission,
		Checkout:        &checkouthttp.Config{Service: f.checkout, Origin: origin, FakeEnabled: true, Environment: "test", Events: receiver},
		QuoteConversion: &bootstrap.QuoteConversionConfig{Runner: f.runner, Quotes: quotes.New(), Orders: orders.NewQuoteCreator()},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRuntime)
	server, err := bootstrap.New(config)
	if err != nil {
		t.Fatal(err)
	}
	csrf := map[string]string{}
	for id, cookie := range cookies {
		r := httptest.NewRequest("GET", origin+"/api/v1/t/"+f.tenant+"/customer/session", nil).WithContext(f.ctx)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		var session struct {
			CSRF string `json:"csrf_token"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &session) != nil || session.CSRF == "" {
			t.Fatalf("session: %d", w.Code)
		}
		csrf[id] = session.CSRF
	}
	request := func(method, path, body, id, etag, token, requestHost string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body)).WithContext(quoteObserved(f.ctx))
		r.Host = requestHost
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("If-Match", etag)
		r.Header.Set("Idempotency-Key", uuid.NewString())
		r.Header.Set("X-CSRF-Token", token)
		r.Header.Set("X-Tenant-ID", f.tenant)
		r.Header.Set("X-Buyer-Company-ID", f.company)
		r.Header.Set("X-Forwarded-Host", host)
		if cookies[id] != nil {
			r.AddCookie(cookies[id])
		} else {
			r.AddCookie(&http.Cookie{Name: "__Host-osc_admin_session", Value: "staff-cookie"})
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, r)
		return w
	}
	check := func(t *testing.T, w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	base := "/api/public/v1/quotes/"
	newQuote := func() qc.Quote {
		var out qc.Quote
		staff := f.mustActor(t, f.staff, "", true)
		err := f.run(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = quotes.New().Create(ctx, tx, staff, qc.CreateInput{SiteID: f.site, InquiryID: f.inquiry, CompanyID: f.company, Currency: "USD", ExpiresAt: time.Now().Add(time.Hour), Lines: []qc.LineInput{{ProductID: f.product, VariantID: f.sku, Description: "B2B", Quantity: 2, UnitPrice: 100, TaxCategory: "standard"}}, Destination: tax.Destination{Country: "US"}, ShippingProfileID: f.profile}, qc.WriteOptions{IfMatch: "*", IdempotencyKey: uuid.NewString()})
			if err != nil {
				return err
			}
			out, err = quotes.New().Transition(ctx, tx, staff, out.ID, qc.Send, qc.WriteOptions{IfMatch: out.ETag(), IdempotencyKey: uuid.NewString()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("inquiry uses verified host", func(t *testing.T) {
		w := request("POST", "/api/public/v1/inquiries", `{"buyer_email":"inquiry@example.test","message":"B2B request"}`, f.buyer, "", csrf[f.buyer], host)
		check(t, w, 202)
		var receipt struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || receipt.Status != "received" {
			t.Fatal("inquiry receipt")
		}
		f.maintenance(t, func(ctx context.Context, tx pgx.Tx) error {
			var site string
			if err := tx.QueryRow(ctx, `SELECT site_id::text FROM app.inquiries WHERE tenant_id=$1 AND id=$2`, f.tenant, receipt.ID).Scan(&site); err != nil {
				return err
			}
			if site != f.site {
				return fmt.Errorf("wrong inquiry site")
			}
			return nil
		})
		check(t, request("POST", "/api/public/v1/inquiries", `{"buyer_email":"inquiry@example.test","message":"request"}`, f.buyer, "", csrf[f.buyer], "unverified.example.invalid"), 404)
	})
	t.Run("buyer lifecycle and negative authority", func(t *testing.T) {
		q := newQuote()
		path := base + q.ID
		check(t, request("GET", path, "", f.buyer, "", "", host), 200)
		check(t, request("POST", path+"/approve", "{}", f.approver, q.ETag(), csrf[f.approver], host), 409)
		check(t, request("POST", path+"/accept", "{}", f.buyer, q.ETag(), "forged", host), 403)
		check(t, request("GET", path, "", outsider, "", "", host), 404)
		check(t, request("POST", path+"/accept", "{}", f.buyer, q.ETag(), csrf[f.buyer], "unverified.example.invalid"), 404)
		w := request("POST", path+"/accept", "{}", f.buyer, q.ETag(), csrf[f.buyer], host)
		check(t, w, 200)
		etag := w.Header().Get("ETag")
		check(t, request("POST", path+"/approve", "{}", purchaser, etag, csrf[purchaser], host), 403)
		check(t, request("POST", path+"/approve", "{}", f.buyer, etag, csrf[f.buyer], host), 403)
		check(t, request("POST", path+"/approve", "{}", outsider, etag, csrf[outsider], host), 404)
		check(t, request("POST", path+"/approve", "{}", "staff", etag, "", host), 401)
		if staffAdmission.calls != 0 {
			t.Fatal("buyer operations called staff authenticator")
		}
		check(t, request("POST", "/api/v1/t/"+f.tenant+"/quotes/"+q.ID+"/orders", "{}", f.buyer, etag, csrf[f.buyer], host), 409)
		w = request("POST", path+"/approve", "{}", f.approver, etag, csrf[f.approver], host)
		check(t, w, 200)
		check(t, request("POST", "/api/v1/t/"+f.tenant+"/quotes/"+q.ID+"/orders", "{}", f.buyer, w.Header().Get("ETag"), csrf[f.buyer], host), 201)
		check(t, request("POST", base+f.quote.ID+"/accept", "{}", f.buyer, f.quote.ETag(), csrf[f.buyer], host), 409)
	})
}
