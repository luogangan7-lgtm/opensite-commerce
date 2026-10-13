// Responsibility: public read boundary regression; dependencies: publish contract.
// Caller: Go gate; invariant: host/path inputs cannot select or navigate unsafe targets.
package contract_test

import (
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	for _, v := range []string{"shop.example", "SHOP.EXAMPLE."} {
		got, err := contract.NormalizeHost(v)
		if err != nil || got != "shop.example" {
			t.Fatalf("%q: %s %v", v, got, err)
		}
	}
	for _, v := range []string{"", "shop.example:443", "https://shop.example", "../shop.example", "shop.example/path", "shop.example%00", " shop.example", "shop..example", "-shop.example", "shop_.example"} {
		if _, err := contract.NormalizeHost(v); err == nil {
			t.Errorf("unsafe hostname accepted: %q", v)
		}
	}
}
func TestSafePath(t *testing.T) {
	for _, v := range []string{"/", "/about", "/products/hearing-aid"} {
		if !contract.SafePath(v) {
			t.Errorf("valid path rejected %q", v)
		}
	}
	for _, v := range []string{"//evil.example", "javascript:alert(1)", "/../secret", "/%2e%2e/secret", "/a\\b", "/a?x=1", "/a#x", "/a//b"} {
		if contract.SafePath(v) {
			t.Errorf("unsafe path accepted %q", v)
		}
	}
}

func TestSafeCMSPath(t *testing.T) {
	for _, v := range []string{"/", "/about", "/guides/setup", "/products-guide", "/404/help"} {
		if !contract.SafeCMSPath(v) {
			t.Errorf("valid CMS path rejected %q", v)
		}
	}
	for _, v := range []string{"/404", "/502", "/products", "/products/aid", "/products/guides/setup", "/a//b", "//evil.example", "/%2fsecret"} {
		if contract.SafeCMSPath(v) {
			t.Errorf("reserved or unsafe CMS path accepted %q", v)
		}
	}
}
