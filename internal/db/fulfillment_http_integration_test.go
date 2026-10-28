//go:build pgx

// Responsibilities: verify fulfillment/returns HTTP composition on real PG18.
// Dependencies: bootstrap, public modules, tenanttx and durable C-04 responses.
// Callers: tagged integration gate. Invariants: fixed test actor projections do
// not implement production authentication; replay cannot duplicate stock/money.
package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/fulfillment"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/returns"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/idempotency"
)

// This substitute has a fixed server-owned actor, never a client identity field.
type fulfillmentHTTPTestAuth struct {
	tenant string
	actor  httpapi.Actor
}

func (a fulfillmentHTTPTestAuth) Authenticate(ctx context.Context, r *http.Request, spec httpapi.RouteSpec) (context.Context, error) {
	if spec.Access != httpapi.AdminOrAPIKey || (spec.Scope != "fulfillment:write" && spec.Scope != "returns:write") {
		return nil, apperrors.New(403, "forbidden", "Test scope denied")
	}
	if r.PathValue("tenant_id") != a.tenant {
		return nil, apperrors.New(404, "not_found", "Test tenant hidden")
	}
	return ctx, nil
}

func (a fulfillmentHTTPTestAuth) ResolveActor(context.Context, *http.Request, httpapi.RouteSpec) (httpapi.Actor, error) {
	return a.actor, nil
}

func TestFulfillmentHTTPPartialShipmentCancellationRefundAndReplay(t *testing.T) {
	f := newFulfillmentFixture(t)
	if f.order.Total.Amount != 460 || f.stock(t) != 16 {
		t.Fatal("fixture must have four paid units and committed stock")
	}
	store, err := idempotency.NewLocalResponseStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	config := bootstrap.Config{
		Fulfillment: fulfillment.HTTPConfig{Run: f.runtime.InTenant, Service: fulfillment.New(), Idempotency: httpapi.CommandConfig{Store: store}},
		Returns:     returns.HTTPConfig{Run: f.runtime.InTenant, Service: f.returns, Idempotency: httpapi.CommandConfig{Store: store}},
	}
	newServer := func(auth httpapi.Authenticator) *httptest.Server {
		t.Helper()
		config.Authenticator = auth
		server, err := bootstrap.New(config)
		if err != nil {
			t.Fatal(err)
		}
		httpServer := httptest.NewServer(server.Handler)
		t.Cleanup(httpServer.Close)
		return httpServer
	}
	actorA := uuid.NewString()
	actorB := uuid.NewString()
	a := newServer(fulfillmentHTTPTestAuth{f.tenant, httpapi.Actor{Kind: "user", ID: actorA}})
	b := newServer(fulfillmentHTTPTestAuth{f.tenant, httpapi.Actor{Kind: "user", ID: actorB}})
	denied := newServer(nil)
	post := func(server *httptest.Server, path, key string, version int64, body []byte, want int) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(f.ctx, http.MethodPost, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", strconv.Quote(strconv.FormatInt(version, 10)))
		request.Header.Set("Idempotency-Key", key)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		value, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("POST %s status=%d want=%d body=%s", path, response.StatusCode, want, value)
		}
		if want < 400 && response.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("command response content type: %s", response.Header.Get("Content-Type"))
		}
		return value
	}
	shipmentPath := fmt.Sprintf("/api/v1/t/%s/orders/%s/shipments", f.tenant, f.order.ID)
	shipmentBody := []byte(fmt.Sprintf(`{"carrier":"manual","tracking_number":"HTTP-1","lines":[{"order_line_id":%q,"quantity":1}]}`, f.line))
	wireKey := "d5287bc5-5649-5093-b02f-c01277d56d30"
	post(denied, shipmentPath, wireKey, f.order.Version, shipmentBody, http.StatusUnauthorized)
	first := post(a, shipmentPath, wireKey, f.order.Version, shipmentBody, http.StatusCreated)
	var shipmentA, shipmentB httpapi.ShipmentRecord
	if err := json.Unmarshal(first, &shipmentA); err != nil {
		t.Fatal(err)
	}
	second := post(b, shipmentPath, wireKey, shipmentA.OrderVersion, shipmentBody, http.StatusCreated)
	if err := json.Unmarshal(second, &shipmentB); err != nil {
		t.Fatal(err)
	}
	if shipmentA.Id == shipmentB.Id || shipmentB.OrderVersion <= shipmentA.OrderVersion || len(shipmentA.Lines) != 1 || len(shipmentB.Lines) != 1 || shipmentA.Lines[0].Quantity != 1 || shipmentB.Lines[0].Quantity != 1 || f.stock(t) != 16 {
		t.Fatal("actor-scoped same wire key must create two partial shipments without changing committed stock")
	}
	if replay := post(a, shipmentPath, wireKey, f.order.Version, shipmentBody, http.StatusCreated); !bytes.Equal(replay, first) {
		t.Fatal("shipment C-04 replay drifted after another actor changed the order")
	}
	changed := bytes.Replace(shipmentBody, []byte(`"quantity":1`), []byte(`"quantity":2`), 1)
	post(a, shipmentPath, wireKey, f.order.Version, changed, http.StatusUnprocessableEntity)
	returnPath := fmt.Sprintf("/api/v1/t/%s/orders/%s/returns", f.tenant, f.order.ID)
	cancelBody := []byte(fmt.Sprintf(`{"kind":"unshipped_cancel","reason":"cancel unshipped remainder","lines":[{"order_line_id":%q,"quantity":2}]}`, f.line))
	// The same actor/wire key is also independent across operation IDs.
	cancelledBytes := post(a, returnPath, wireKey, shipmentB.OrderVersion, cancelBody, http.StatusCreated)
	var cancelled httpapi.ReturnRecord
	if err := json.Unmarshal(cancelledBytes, &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "received" || !cancelled.Restocked || cancelled.RefundMinor != "230" || f.stock(t) != 18 {
		t.Fatalf("unshipped cancellation must restore two units: return=%+v stock=%d", cancelled, f.stock(t))
	}
	refundPath := fmt.Sprintf("/api/v1/t/%s/returns/%s/refund", f.tenant, cancelled.Id)
	refundKey := "4d896c92-f59b-5627-b3cf-9d4819e01279"
	refundedBytes := post(a, refundPath, refundKey, cancelled.OrderVersion, []byte(`{}`), http.StatusOK)
	var refunded httpapi.ReturnRecord
	if err := json.Unmarshal(refundedBytes, &refunded); err != nil {
		t.Fatal(err)
	}
	if refunded.Status != "refunded" || refunded.RefundMinor != "230" || refunded.OrderVersion <= cancelled.OrderVersion {
		t.Fatalf("refund response does not reflect financial completion: %+v", refunded)
	}
	if replay := post(a, refundPath, refundKey, cancelled.OrderVersion, []byte(`{}`), http.StatusOK); !bytes.Equal(replay, refundedBytes) {
		t.Fatal("refund old If-Match C-04 replay must preserve the exact successful bytes")
	}
	if replay := post(a, returnPath, wireKey, shipmentB.OrderVersion, cancelBody, http.StatusCreated); !bytes.Equal(replay, cancelledBytes) {
		t.Fatal("return creation replay drifted after refund")
	}
	var state string
	var paidBack, shipments, shippedQuantity, journals, debits, credits, completed, actors int64
	err = f.runtime.InTenant(f.ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT state,refunded_minor FROM app.orders WHERE tenant_id=$1 AND id=$2", f.tenant, f.order.ID).Scan(&state, &paidBack); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(DISTINCT s.id),sum(l.quantity)::bigint FROM app.shipments s JOIN app.shipment_lines l ON l.tenant_id=s.tenant_id AND l.shipment_id=s.id WHERE s.tenant_id=$1 AND s.order_id=$2", f.tenant, f.order.ID).Scan(&shipments, &shippedQuantity); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(DISTINCT transaction_id),coalesce(sum(amount) FILTER(WHERE side='debit'),0)::bigint,coalesce(sum(amount) FILTER(WHERE side='credit'),0)::bigint FROM app.ledger_entries WHERE tenant_id=$1", f.tenant).Scan(&journals, &debits, &credits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT count(*),count(DISTINCT actor_id) FROM app.idempotency_records WHERE tenant_id=$1 AND state='completed' AND operation IN ('createShipment','createReturn','refundReturn')", f.tenant).Scan(&completed, &actors)
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != "partially_refunded" || paidBack != 230 || shipments != 2 || shippedQuantity != 2 || journals != 1 || debits != 230 || credits != 230 || completed != 4 || actors != 2 || len(f.payments.Events()) != 3 || f.stock(t) != 18 {
		t.Fatalf("HTTP/PG conservation state=%s refund=%d shipments=%d quantity=%d journals=%d debit=%d credit=%d receipts=%d actors=%d PSPevents=%d stock=%d", state, paidBack, shipments, shippedQuantity, journals, debits, credits, completed, actors, len(f.payments.Events()), f.stock(t))
	}
}
