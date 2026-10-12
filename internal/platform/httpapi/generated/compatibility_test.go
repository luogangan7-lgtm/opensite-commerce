// Responsibilities: exercise generated OAS 3.1 unions, three-state nullable fields,
// and RFC 9457 typed response serialization. Depends only on generated DTOs and
// stdlib; check_all/CI call it. This POC registers no production business routes.
package generated_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	gen "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/generated"
)

// Test-only strict fixture: its other methods are never reached. No handlers
// or generated mux from this POC are installed in production bootstrap.
type strictFixture struct{ gen.StrictServerInterface }

func (strictFixture) Health(context.Context, gen.HealthRequestObject) (gen.HealthResponseObject, error) {
	return gen.Health204Response{}, nil
}

func TestStrictNetHTTPServer(t *testing.T) {
	var fixture gen.StrictServerInterface = strictFixture{}
	handler := gen.Handler(gen.NewStrictHandler(fixture, nil))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != 204 || recorder.Body.Len() != 0 {
		t.Fatalf("strict net/http mapping: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestOAS31OneOf(t *testing.T) {
	for _, kind := range []string{"component.add", "component.move", "component.set_props"} {
		body := `{"operation_id":"00000000-0000-0000-0000-000000000001","type":"` + kind + `","target":{"page_key":"home","node_id":"n"},"arguments":{"component":"text@1","node_id":"n","parent_node_id":"root","slot":"main","props":{}}}`
		var union gen.PageOperation
		if err := json.Unmarshal([]byte(body), &union); err != nil {
			t.Fatal(err)
		}
		disc, err := union.Discriminator()
		if err != nil || disc != kind {
			t.Fatalf("discriminator: %q %v", disc, err)
		}
		if _, err := union.ValueByDiscriminator(); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(union)
		if err != nil || !json.Valid(out) {
			t.Fatalf("union marshal: %s %v", out, err)
		}
	}
	var unknown gen.PageOperation
	if err := json.Unmarshal([]byte(`{"type":"unknown"}`), &unknown); err != nil {
		t.Fatal(err)
	}
	if _, err := unknown.ValueByDiscriminator(); err == nil {
		t.Fatal("unknown union discriminator accepted")
	}
}

func TestOAS31Nullable(t *testing.T) {
	for _, tc := range []struct {
		body            string
		specified, null bool
	}{
		{`{}`, false, false},
		{`{"base_release_id":null}`, true, true},
		{`{"base_release_id":"00000000-0000-0000-0000-000000000001"}`, true, false},
	} {
		var draft gen.CreateDraft
		if err := json.Unmarshal([]byte(tc.body), &draft); err != nil {
			t.Fatal(err)
		}
		if draft.BaseReleaseId.IsSpecified() != tc.specified || draft.BaseReleaseId.IsNull() != tc.null {
			t.Fatalf("lost nullable state: %s", tc.body)
		}
		out, err := json.Marshal(draft)
		if err != nil || string(out) != tc.body {
			t.Fatalf("nullable roundtrip: %s %v", out, err)
		}
	}
}

func TestTypedProblemResponse(t *testing.T) {
	var response gen.ApplyPageOperations412ApplicationProblemPlusJSONResponse
	if err := json.Unmarshal([]byte(`{"type":"about:blank","title":"Precondition failed","status":412,"code":"etag_mismatch"}`), &response); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := response.VisitApplyPageOperationsResponse(recorder); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 412 || recorder.Header().Get("Content-Type") != "application/problem+json" || body["status"] != float64(412) || body["code"] != "etag_mismatch" {
		t.Fatalf("problem mapping: %d %v %v", recorder.Code, recorder.Header(), body)
	}
}

func TestGeneratedDTOAdmission(t *testing.T) {
	// Generated strict interfaces constrain envelopes, not JSON schema admission.
	// The existing platform decoder must precede generated decoding in transports.
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"product_code":"SKU1"}`, true},
		{`{"product_code":"SKU1","undeclared":true}`, false},
		{`{"product_code":"SKU1"} {}`, false},
	} {
		req := httptest.NewRequest("POST", "/probe", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		var dto gen.CreateProduct
		err := httpapi.DecodeJSON(httptest.NewRecorder(), req, &dto)
		if (err == nil) != tc.valid {
			t.Fatalf("generated DTO admission valid=%v: %v", tc.valid, err)
		}
	}
}

func TestOneOfSchemaAdmission(t *testing.T) {
	doc, err := gen.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas["PageOperation"].Value
	valid := `{"operation_id":"00000000-0000-0000-0000-000000000001","type":"component.move","target":{"page_key":"home","node_id":"n"},"arguments":{"parent_node_id":"root","slot":"main"}}`
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{valid, true},
		{strings.Replace(valid, `"type":"component.move"`, `"type":"unknown"`, 1), false},
		{strings.Replace(valid, `"parent_node_id":"root",`, "", 1), false},
		{strings.Replace(valid, `"slot":"main"`, `"slot":"main","undeclared":true`, 1), false},
	} {
		var value any
		if err := json.Unmarshal([]byte(tc.body), &value); err != nil {
			t.Fatal(err)
		}
		err := schema.VisitJSON(value)
		if (err == nil) != tc.valid {
			t.Fatalf("oneOf schema admission valid=%v: %v", tc.valid, err)
		}
	}
}
