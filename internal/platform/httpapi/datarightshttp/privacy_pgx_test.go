//go:build pgx

// Responsibilities: prove HTTP replay, native River enqueue, failure rollback,
// signed worker duplicates and capability custody against disposable real PG.
// Dependencies: approved pgxadapter transactions and public service/contracts;
// focused integration gate runs with C22A_DATABASE_URL, no raw connections.
package datarightshttp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights"
	rights "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/datarights/contract"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/datarightshttp"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/jobs"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/pgxtest"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx/pgxadapter"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type pgAuth struct {
	tenant, actor string
	scope         string
}

func (a pgAuth) Authenticate(ctx context.Context, r *http.Request, s httpapi.RouteSpec) (context.Context, error) {
	if r.PathValue("tenant_id") != a.tenant {
		return nil, apperrors.New(404, "not_found", "Tenant not found")
	}
	if s.Access != httpapi.AdminOrAPIKey || s.Scope != a.scope {
		return nil, apperrors.New(403, "forbidden", "Denied")
	}
	return context.WithValue(ctx, subjectKey{}, httpapi.Subject{Kind: "api_key", ID: a.actor}), nil
}

type cleanupFailure struct {
	rights.Service
	failures int
}

func (s *cleanupFailure) Cleanup(ctx context.Context, tenant string, ids []string) error {
	if s.failures > 0 {
		s.failures--
		return errors.New("synthetic cleanup failure")
	}
	return s.Service.Cleanup(ctx, tenant, ids)
}
func TestPrivacyHTTPPGAtomicReplaySignedJobs(t *testing.T) {
	dsn := os.Getenv("C22A_DATABASE_URL")
	if dsn == "" {
		pgxtest.Skip(t, "NOT_RUN: dedicated disposable C22A_DATABASE_URL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	maintenanceDSN := os.Getenv("C22A_MAINTENANCE_DATABASE_URL")
	if maintenanceDSN == "" {
		t.Fatal("C22A_MAINTENANCE_DATABASE_URL required")
	}
	admin, closeAdmin, err := pgxadapter.Open(ctx, maintenanceDSN)
	if err != nil {
		t.Fatal("fixture database unavailable")
	}
	defer closeAdmin()
	tenantID, customerID, actorID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	login := "c22a_http_" + strings.ReplaceAll(tenantID[:8], "-", "")
	password := strings.ReplaceAll(uuid.NewString(), "-", "")
	// Register cleanup immediately after role creation; credentials never enter logs.
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{login}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT PASSWORD '"+password+"'")
		return err
	}); err != nil {
		t.Fatal("fixture login creation failed")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := admin.InMaintenance(cleanup, func(ctx context.Context, tx pgx.Tx) error {
			// Operator-only cleanup of this unique synthetic fixture; journaling stays
			// immutable for every application transaction under test.
			if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role=replica"); err != nil {
				return err
			}
			for _, query := range []string{"DELETE FROM queue.river_job WHERE args->>'tenant_id'=$1", "DELETE FROM app.data_rights_exports WHERE tenant_id=$1::uuid", "DELETE FROM app.data_rights_erasures WHERE tenant_id=$1::uuid", "DELETE FROM app.idempotency_records WHERE tenant_id=$1::uuid", "DELETE FROM app.audit_events WHERE tenant_id=$1::uuid", "DELETE FROM app.customers WHERE tenant_id=$1::uuid", "DELETE FROM app.tenants WHERE id=$1::uuid"} {
				if _, err := tx.Exec(ctx, query, tenantID); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, "DROP ROLE "+pgx.Identifier{login}.Sanitize())
			return err
		}); err != nil {
			t.Errorf("fixture cleanup: %v", err)
		}
	}()
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "GRANT app_runtime TO "+pgx.Identifier{login}.Sanitize()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO app.tenants(id,name) VALUES($1::uuid,'C22a HTTP fixture')", tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO app.customers(tenant_id,id,email,display_name) VALUES($1::uuid,$2::uuid,'synthetic-c22a@example.invalid','Synthetic C22a')", tenantID, customerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runtimeURL, err := url.Parse(maintenanceDSN)
	if err != nil || runtimeURL.Scheme == "" {
		t.Fatal("fixture URI config required")
	}
	runtimeURL.User = url.UserPassword(login, password)
	query := runtimeURL.Query()
	query.Del("role")
	query.Del("options")
	runtimeURL.RawQuery = query.Encode()
	runtime, closeRuntime, err := pgxadapter.Open(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal("restricted runtime login unavailable")
	}
	defer closeRuntime()
	var lastError error
	run := func(ctx context.Context, tenant string, fn func(context.Context, pgx.Tx) error) error {
		lastError = runtime.InTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
				return err
			}
			var session, effective string
			var super, bypass bool
			if err := tx.QueryRow(ctx, "SELECT session_user,current_user,rolsuper,rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&session, &effective, &super, &bypass); err != nil {
				return err
			}
			if session != login || effective != "app_runtime" || super || bypass {
				return errors.New("restricted runtime witness failed")
			}
			return fn(ctx, tx)
		})
		return lastError
	}
	root := t.TempDir()
	svc, err := datarights.New(rights.Config{Run: run, Root: root, TokenKey: []byte("synthetic-c22a-token-key-32bytes!!")})
	if err != nil {
		t.Fatal(err)
	}
	responses, err := idempotency.NewLocalResponseStore(filepath.Join(root, "responses"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := jobs.NewSigner([]byte("synthetic-c22a-job-sign-key-32bytes"))
	producer, err := jobs.NewEnqueuer(signer)
	if err != nil {
		t.Fatal(err)
	}
	failEnqueue := false
	cleanupService := &cleanupFailure{Service: svc}
	cfg := datarightshttp.Config{Service: cleanupService, SubjectCommandConfig: httpapi.SubjectCommandConfig{Run: run, Subject: subject, Responses: responses}, Enqueue: func(ctx context.Context, tx pgx.Tx, id string) error {
		if failEnqueue {
			return producer.CustomerExportTx(ctx, tx, "invalid")
		}
		return producer.CustomerExportTx(ctx, tx, id)
	}}
	router := httpapi.New(pgAuth{tenant: tenantID, actor: actorID, scope: "customers:privacy"}, nil)
	for _, route := range datarightshttp.New(cfg).Routes() {
		if err := router.Register(route.Spec, route.Handler); err != nil {
			t.Fatal(err)
		}
	}
	urlBase := "/api/v1/t/" + tenantID + "/customers/" + customerID + "/data-exports"
	request := func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	first := request("POST", urlBase, "original")
	if first.Code != 202 {
		t.Fatalf("export request=%d %s callback=%v", first.Code, first.Body.String(), lastError)
	}
	var created struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if json.Unmarshal(first.Body.Bytes(), &created) != nil || created.RequestID == "" || created.Status != "pending" || strings.Contains(first.Body.String(), "token") {
		t.Fatal("POST leaked capability or invalid metadata")
	}
	replay := request("POST", urlBase, "original")
	if replay.Code != 202 || replay.Body.String() != first.Body.String() {
		t.Fatal("exact replay failed")
	}
	changed := request("POST", strings.Replace(urlBase, customerID, uuid.NewString(), 1), "original")
	if changed.Code != 422 {
		t.Fatalf("fingerprint mismatch status=%d", changed.Code)
	}
	failEnqueue = true
	failed := request("POST", urlBase, "enqueue-failure")
	failEnqueue = false
	if failed.Code != 500 {
		t.Fatalf("enqueue failure status=%d", failed.Code)
	}
	var args jobs.CustomerDataExportArgs
	var jobID int64
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var exports, queued, claims int
		var raw []byte
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM app.data_rights_exports WHERE tenant_id=$1::uuid", tenantID).Scan(&exports); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM queue.river_job WHERE kind='customer_data_export' AND args->>'tenant_id'=$1", tenantID).Scan(&queued); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM app.idempotency_records WHERE tenant_id=$1::uuid", tenantID).Scan(&claims); err != nil {
			return err
		}
		if exports != 1 || queued != 1 || claims != 1 {
			return errors.New("enqueue/domain/idempotency rollback or replay failed")
		}
		if err := tx.QueryRow(ctx, "SELECT id,args FROM queue.river_job WHERE kind='customer_data_export' AND args->>'tenant_id'=$1", tenantID).Scan(&jobID, &raw); err != nil {
			return err
		}
		return json.Unmarshal(raw, &args)
	}); err != nil {
		t.Fatal(err)
	}
	worker := jobs.CustomerExportWorker{Signer: signer, Service: svc}
	job := &river.Job[jobs.CustomerDataExportArgs]{JobRow: &rivertype.JobRow{ID: jobID, Kind: "customer_data_export"}, Args: args}
	if err = worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "data-rights", tenantID, created.RequestID+".json")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(data)
	if err = worker.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	after := sha256.Sum256(data)
	if before != after {
		t.Fatal("duplicate worker changed export")
	}
	for _, field := range []string{"tenant", "request"} {
		forged := *job
		forged.Args = job.Args
		if field == "tenant" {
			forged.Args.TenantID = uuid.NewString()
		} else {
			forged.Args.RequestID = uuid.NewString()
		}
		if worker.Work(ctx, &forged) == nil {
			t.Fatalf("forged %s admitted", field)
		}
	}
	status := request("GET", urlBase+"/"+created.RequestID, "")
	if status.Code != 200 {
		t.Fatalf("status=%d %s", status.Code, status.Body.String())
	}
	var view struct {
		DownloadURL       string    `json:"download_url"`
		DownloadExpiresAt time.Time `json:"download_expires_at"`
	}
	if json.Unmarshal(status.Body.Bytes(), &view) != nil || view.DownloadURL == "" || view.DownloadExpiresAt.IsZero() || time.Until(view.DownloadExpiresAt) > 15*time.Minute {
		t.Fatal("short-lived capability not projected")
	}
	download := request("GET", view.DownloadURL, "")
	if download.Code != 200 || !strings.Contains(download.Body.String(), "synthetic-c22a@example.invalid") {
		t.Fatalf("download=%d", download.Code)
	}
	if again := request("GET", view.DownloadURL, ""); again.Code != 409 {
		t.Fatalf("once-only download=%d", again.Code)
	}
	// Cleanup failure occurs after commit; replay retries deletion from the
	// stored opaque IDs without rerunning erasure or exposing profile data.
	cleanupService.failures = 1
	eraseURL := "/api/v1/t/" + tenantID + "/customers/" + customerID + "/data-erasure"
	erasedFailure := request("POST", eraseURL, "erase-original")
	if erasedFailure.Code != 503 {
		t.Fatalf("cleanup failure=%d %s", erasedFailure.Code, erasedFailure.Body.String())
	}
	if _, err = os.Stat(filename); err != nil {
		t.Fatal("fixture did not preserve failed cleanup artifact")
	}
	erased := request("POST", eraseURL, "erase-original")
	if erased.Code != 200 {
		t.Fatalf("erase replay=%d %s", erased.Code, erased.Body.String())
	}
	if _, err = os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("erase replay did not clean export")
	}
	if err = admin.InMaintenance(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var auditCount int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM app.audit_events WHERE tenant_id=$1::uuid AND operation='data_rights.customer.erase'", tenantID).Scan(&auditCount); err != nil {
			return err
		}
		if auditCount != 1 {
			return errors.New("erase replay duplicated audit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Response blobs contain only request metadata, never profile or capability.
	token := strings.Split(view.DownloadURL, "token=")[1]
	if err = filepath.WalkDir(filepath.Join(root, "responses"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(body), token) || strings.Contains(string(body), "synthetic-c22a@example.invalid") {
				return errors.New("PII or token in replay store")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queue, err := pgxadapter.OpenQueuePool(ctx, maintenanceDSN, "app_queue_worker")
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	err = pgxadapter.InQueueTenant(ctx, queue, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT app.data_rights_build_export($1::uuid)", created.RequestID)
		return err
	})
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
		t.Fatal("queue role unexpectedly has privacy capability")
	}
	t.Logf("metadata replay+rollback/native signed job+duplicate/blobSHA256=%s/15m once-only capability/restricted roles verified", hex.EncodeToString(after[:]))
}
