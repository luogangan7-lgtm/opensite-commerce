// Quote public-port safety tests; dependencies: pure calculators and contract.
// Invariants: no database proof claimed here; frozen amounts and fail-closed actors.
package quotes_test

import (
	"context"
	"encoding/json"
	"strings"
	"errors"
	q "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/shipping"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tax"
	"math"
	"testing"
	"time"
)

func sample() (c.CreateInput, c.CalculationRules, time.Time) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	return c.CreateInput{SiteID: "11111111-1111-1111-1111-111111111111", InquiryID: "22222222-2222-2222-2222-222222222222", CompanyID: "33333333-3333-3333-3333-333333333333", Currency: "USD", ExpiresAt: at.Add(time.Hour), Destination: tax.Destination{Country: "US"}, ShippingProfileID: "profile", Lines: []c.LineInput{{Description: "negotiated", Quantity: 2, UnitPrice: 100, TaxCategory: "standard"}}}, c.CalculationRules{Tax: tax.Ruleset{Zones: []tax.Zone{{ID: "zone", Locations: []tax.Destination{{Country: "US"}}}}, Categories: []tax.Category{{Code: "standard"}}, Rates: []tax.Rate{{ID: "rate", ZoneID: "zone", Category: "standard", BasisPoints: 1000, ValidFrom: at.Add(-time.Hour)}}}, ShippingZones: []shipping.Zone{{ID: "zone", Destinations: []shipping.Destination{{Country: "US"}}}}, ShippingRates: []shipping.Rate{{ID: "ship", ZoneID: "zone", ProfileID: "profile", Method: "fixed", Currency: "USD", Amount: 50}}}, at
}
func TestFrozenSnapshot(t *testing.T) {
	in, r, at := sample()
	got, e := q.CalculateSnapshot(in, r, at)
	if e != nil {
		t.Fatal(e)
	}
	if got.Subtotal != 200 || got.TaxAmount != 20 || got.ShippingAmount != 50 || got.Total != 270 {
		t.Fatalf("amounts %+v", got)
	}
	in.Lines[0].UnitPrice = 999
	r.Tax.Rates[0].BasisPoints = 999
	if got.Lines[0].UnitPrice != 100 || got.Tax.Lines[0].Snapshot.BasisPoints != 1000 {
		t.Fatal("mutable snapshot")
	}
}
func TestSnapshotInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*c.CreateInput)
		want   error
	}{{"expiry", func(x *c.CreateInput) { x.ExpiresAt = x.ExpiresAt.Add(-time.Hour) }, c.ErrInvalid}, {"currency", func(x *c.CreateInput) { x.Currency = "usd" }, c.ErrInvalid}, {"overflow", func(x *c.CreateInput) { x.Lines[0].UnitPrice = math.MaxInt64 }, c.ErrOverflow}, {"negative", func(x *c.CreateInput) { x.Lines[0].Quantity = -1 }, c.ErrInvalid}} {
		t.Run(tc.name, func(t *testing.T) {
			in, r, at := sample()
			tc.change(&in)
			if _, e := q.CalculateSnapshot(in, r, at); !errors.Is(e, tc.want) {
				t.Fatalf("got %v want %v", e, tc.want)
			}
		})
	}
}
func TestActorDeny(t *testing.T) {
	if _, e := c.ActorFromContext(context.Background()); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal(e)
	}
	if _, e := q.New().Get(context.Background(), nil, c.AuthorizedActor{}, "11111111-1111-1111-1111-111111111111"); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal(e)
	}
}
func TestSelfApprovalAndState(t *testing.T) {
	for _, tc := range []struct {
		state              string
		action             c.Action
		kind, id, accepted string
		expiry             bool
		want               error
	}{{"accepted_pending_approval", c.Approve, "customer", "a", "a", false, c.ErrForbidden}, {"draft", c.Accept, "customer", "a", "", false, c.ErrConflict}, {"sent", c.Accept, "customer", "a", "", true, c.ErrConflict}, {"approved", c.Send, "staff", "s", "a", false, c.ErrConflict}, {"sent", c.Accept, "customer", "a", "", false, nil}, {"accepted_pending_approval", c.Approve, "customer", "b", "a", false, nil}} {
		at := time.Now()
		exp := at.Add(time.Hour)
		if tc.expiry {
			exp = at
		}
		if e := q.ValidateTransition(c.Quote{Status: tc.state, ExpiresAt: exp, AcceptedBy: tc.accepted}, tc.action, tc.kind, tc.id, at); !errors.Is(e, tc.want) {
			t.Fatalf("%+v got %v", tc, e)
		}
	}
}
func TestIdentityAndApplicationFailClosed(t *testing.T) {
	tid := "11111111-1111-1111-1111-111111111111"
	sid := "22222222-2222-2222-2222-222222222222"
	for _, identity := range []c.ActorIdentity{{TenantID: tid}, {TenantID: tid, StaffID: sid, CustomerID: sid, CompanyID: sid}, {TenantID: tid, CustomerID: sid}, {TenantID: tid, StaffID: sid, CompanyID: sid}} {
		if _, e := c.ActorFromContext(c.WithTrustedActor(context.Background(), identity)); !errors.Is(e, c.ErrUnauthorized) {
			t.Fatalf("identity accepted: %+v %v", identity, e)
		}
	}
	app := q.NewApplication(nil)
	if _, e := app.Create(context.Background(), c.CreateInput{}, c.WriteOptions{}); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal(e)
	}
	if _, e := app.SubmitInquiry(context.Background(), c.InquiryInput{}); !errors.Is(e, c.ErrUnauthorized) {
		t.Fatal(e)
	}
	ctx := c.WithTrustedInquiry(context.Background(), tid, sid)
	if _, e := app.SubmitInquiry(ctx, c.InquiryInput{SiteID: tid}); !errors.Is(e, c.ErrForbidden) {
		t.Fatal(e)
	}
	if _, e := app.SubmitInquiry(ctx, c.InquiryInput{SiteID: sid}); !errors.Is(e, c.ErrUnavailable) {
		t.Fatal(e)
	}
}
func TestCalculatorBoundaryAndInclusive(t *testing.T) {
	in, r, at := sample()
	r.IncludeTax = true
	in.Lines[0].UnitPrice = 110
	snap, e := q.CalculateSnapshot(in, r, at)
	if e != nil || snap.Subtotal != 200 || snap.TaxAmount != 20 || snap.Total != 270 || snap.Lines[0].LineTotal != 200 {
		t.Fatalf("inclusive snapshot %+v err=%v", snap, e)
	}
	in, r, at = sample()
	r.ShippingRates[0].Amount = math.MaxInt64
	if _, e := q.CalculateSnapshot(in, r, at); !errors.Is(e, c.ErrOverflow) {
		t.Fatalf("total overflow %v", e)
	}
	in, r, at = sample()
	r.ShippingRates[0].Currency = "EUR"
	if _, e := q.CalculateSnapshot(in, r, at); !errors.Is(e, c.ErrInvalid) {
		t.Fatalf("currency mismatch %v", e)
	}
}
func TestWorstCaseHundredLineQuoteFitsStoredBound(t *testing.T) {
	in, r, at := sample()
	in.Lines = nil
	// Go escapes '<' and control runes as six bytes; this is the widest legal description.
	for i := range c.MaxQuoteLines {
		in.Lines = append(in.Lines, c.LineInput{ProductID: "55555555-5555-5555-5555-555555555555", VariantID: "66666666-6666-6666-6666-666666666666", Description: strings.Repeat("<\x01", 500), Quantity: math.MaxInt32 + int64(i), UnitPrice: 1 << 20, TaxCategory: "standard"})
	}
	snap, e := q.CalculateSnapshot(in, r, at)
	if e != nil {
		t.Fatal(e)
	}
	data, e := json.Marshal(c.Quote{ID: in.SiteID, TenantID: in.SiteID, SiteID: in.SiteID, InquiryID: in.InquiryID, CompanyID: in.CompanyID, Status: "accepted_pending_approval", Version: math.MaxInt64, Currency: "USD", ExpiresAt: at, CreatedBy: in.SiteID, AcceptedBy: in.SiteID, Snapshot: snap})
	if e != nil {
		t.Fatal(e)
	}
	if len(data) > c.MaxStoredJSONBytes || c.MaxStoredJSONBytes > 1<<20 {
		t.Fatalf("worst-case quote %d bytes exceeds derived bound %d (DB hard cap 1 MiB)", len(data), c.MaxStoredJSONBytes)
	}
	t.Logf("worst-case 100-line quote JSON: %d bytes of %d", len(data), c.MaxStoredJSONBytes)
}
