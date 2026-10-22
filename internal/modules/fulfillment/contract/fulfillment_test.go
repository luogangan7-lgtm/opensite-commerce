// Responsibilities: reject malformed shipment command shapes before SQL.
// Dependencies: public contract; callers: Go gate. Quantities remain DB-capped.
package contract

import "testing"

func TestCreateInputValidation(t *testing.T) {
	base := CreateInput{OrderID: "11111111-1111-4111-8111-111111111111", ExpectedVersion: 2, IdempotencyKey: "ship", Carrier: "manual", Lines: []Line{{OrderLineID: "22222222-2222-4222-8222-222222222222", Quantity: 1}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CreateInput){"zero version": func(i *CreateInput) { i.ExpectedVersion = 0 }, "zero quantity": func(i *CreateInput) { i.Lines[0].Quantity = 0 }, "duplicate line": func(i *CreateInput) { i.Lines = append(i.Lines, i.Lines[0]) }, "header control": func(i *CreateInput) { i.Carrier = "evil\r\n" }, "invalid id": func(i *CreateInput) { i.OrderID = "wrong" }, "key whitespace": func(i *CreateInput) { i.IdempotencyKey = " " }} {
		t.Run(name, func(t *testing.T) {
			in := base
			in.Lines = append([]Line(nil), base.Lines...)
			mutate(&in)
			if in.Validate() == nil {
				t.Fatal("malformed shipment accepted")
			}
		})
	}
}
