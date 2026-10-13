// Responsibility: address/contact limits match the OpenAPI maxLength (characters,
// not bytes). Dependencies: checkout contract only; no database.
package service

import (
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/checkout/contract"
	orders "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/orders/contract"
)

func TestCheckedInputUsesContractCharacterLimits(t *testing.T) {
	actor, err := contract.NewAuthorizedActor(tenant, "guest", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	base := contract.StartInput{CartID: cartID, IdempotencyKey: "k", BuyerContact: orders.Contact{Email: "b@example.test"},
		ShippingAddress: contract.ShippingAddress{Country: "US", Address1: "1 Road", City: "City", PostalCode: "1"}}
	fields := []struct {
		name  string
		limit int
		set   func(*contract.StartInput, string)
	}{
		{"subdivision", 100, func(in *contract.StartInput, v string) { in.ShippingAddress.Subdivision = v }},
		{"address1", 500, func(in *contract.StartInput, v string) { in.ShippingAddress.Address1 = v }},
		{"address2", 500, func(in *contract.StartInput, v string) { in.ShippingAddress.Address2 = v }},
		{"city", 200, func(in *contract.StartInput, v string) { in.ShippingAddress.City = v }},
		{"postal_code", 32, func(in *contract.StartInput, v string) { in.ShippingAddress.PostalCode = v }},
		{"name", 200, func(in *contract.StartInput, v string) { in.BuyerContact.Name = v }},
		{"phone", 64, func(in *contract.StartInput, v string) { in.BuyerContact.Phone = v }},
	}
	for _, f := range fields {
		for _, ch := range []string{"a", "路"} {
			for n, ok := range map[int]bool{f.limit: true, f.limit + 1: false} {
				in := base
				f.set(&in, strings.Repeat(ch, n))
				if got := checkedInput(actor, in); (got == nil) != ok {
					t.Errorf("%s %q x%d: err=%v want ok=%v", f.name, ch, n, got, ok)
				}
			}
		}
	}
}
