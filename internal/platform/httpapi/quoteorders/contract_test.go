// Responsibilities: gate the quote conversion wire contract independently of DTOs.
// Dependencies: existing OpenAPI loader; callers: focused HTTP tests and CI.
// Invariants: conversion accepts only {}, exposes exact money strings, no identity/key.
package quoteorders_test

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestQuoteOrderOpenAPIContract(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ path, method, op, access string }{
		{"/api/v1/t/{tenant_id}/quotes/{quote_id}/orders", "POST", "createQuoteOrder", "BuyerCompany"},
		{"/api/v1/t/{tenant_id}/quotes/{quote_id}/order-conversion", "GET", "getQuoteOrderConversion", "AdminOrAPIKey"},
	} {
		p := doc.Paths.Value(x.path)
		if p == nil {
			t.Fatalf("missing quote conversion path %s", x.path)
		}
		o := p.GetOperation(x.method)
		if o == nil || o.OperationID != x.op || o.Extensions["x-opensite-access"] != x.access {
			t.Fatalf("incorrect operation contract: %+v", o)
		}
		if x.method == "POST" {
			if o.Extensions["x-opensite-trusted-buyer"] != "required" {
				t.Fatal("buyer resolver requirement missing")
			}
			s := o.RequestBody.Value.Content["application/json"].Schema.Value
			if err := s.VisitJSON(map[string]any{}); err != nil {
				t.Fatal(err)
			}
			if err := s.VisitJSON(map[string]any{"amount": "1"}); err == nil {
				t.Fatal("request accepts client money")
			}
		}
	}
	s := doc.Components.Schemas["QuoteOrderConversion"]
	if s == nil {
		t.Fatal("missing conversion response schema")
	}
	for _, k := range []string{"company_id", "customer_id", "idempotency_key"} {
		if s.Value.Properties[k] != nil {
			t.Fatalf("conversion response leaks %s", k)
		}
	}
	for _, k := range []string{"amount", "quote_version", "order_number"} {
		if p := s.Value.Properties[k]; p == nil || !p.Value.Type.Is("string") {
			t.Fatalf("%s must be an exact decimal string", k)
		}
	}
}
