// Responsibilities: reject malformed return references/quantities before SQL.
// Dependencies: public contract; callers: Go gate. Shipment IDs are mandatory
// for physical returns and forbidden for unshipped cancellation.
package contract

import "testing"

func TestRequestValidation(t *testing.T) {
	base := RequestInput{OrderID: "11111111-1111-4111-8111-111111111111", ExpectedVersion: 2, IdempotencyKey: "return", Reason: "not needed", Kind: ShipmentReturn, Lines: []Line{{OrderLineID: "22222222-2222-4222-8222-222222222222", ShipmentLineID: "33333333-3333-4333-8333-333333333333", Quantity: 1}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RequestInput){"shipment missing": func(i *RequestInput) { i.Lines[0].ShipmentLineID = "" }, "unshipped shipment": func(i *RequestInput) { i.Kind = UnshippedCancel }, "duplicate": func(i *RequestInput) { i.Lines = append(i.Lines, i.Lines[0]) }, "negative": func(i *RequestInput) { i.Lines[0].Quantity = -1 }, "missing reason": func(i *RequestInput) { i.Reason = " " }, "unknown kind": func(i *RequestInput) { i.Kind = "exchange" }} {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Lines = append([]Line(nil), base.Lines...)
			mutate(&in)
			if in.Validate() == nil {
				t.Fatal("malformed return accepted")
			}
		})
	}
	base.Kind = UnshippedCancel
	base.Lines[0].ShipmentLineID = ""
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}
