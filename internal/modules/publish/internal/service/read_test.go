// Responsibility: validate immutable manifest/page projection; caller: Go gate.
// Dependencies: service/contract; invariant: invalid snapshots cannot reach renderer.
package service

import (
	"encoding/json"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"testing"
)

func TestCMSRouteOwnership(t *testing.T) {
	for _, tc := range []struct {
		path    string
		allowed bool
	}{
		{"/", true}, {"/about", true}, {"/guides/setup", true},
		{"/products-guide", true}, {"/404/help", true},
		{"/404", false}, {"/502", false}, {"/products", false},
		{"/products/aid", false}, {"/products/guides/setup", false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			manifest, err := json.Marshal(map[string]any{
				"version": 1, "locale": "en", "title": "Shop", "description": "",
				"navigation": []contract.Navigation{{Label: "Page", Href: tc.path}},
				"products":   []contract.Product{{ID: "p", Slug: "aid", Name: "Aid", Description: "", UnitAmount: "100", Currency: "USD"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			pages, err := json.Marshal([]contract.Page{{Path: tc.path, Title: "Published CMS", Description: "", Blocks: []contract.Block{}}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := Project("s", "r", "shop.example", "en", manifest, pages)
			if tc.allowed {
				if err != nil || len(got.Pages) != 1 || got.Pages[0].Path != tc.path {
					t.Fatalf("valid published CMS route rejected: %+v %v", got, err)
				}
			} else if !errors.Is(err, contract.ErrUnavailable) {
				t.Fatalf("reserved CMS route accepted: %+v %v", got, err)
			}
		})
	}
}

func TestProjection(t *testing.T) {
	manifest := []byte(`{"version":1,"locale":"en","title":"Store","description":"Public","navigation":[{"label":"About","href":"/about"}],"products":[{"id":"p1","slug":"aid","name":"Aid","description":"Text","unit_amount":"9007199254740993","currency":"USD"}]}`)
	pages := []byte(`[{"path":"/about","title":"About","description":"Text","blocks":[{"kind":"heading","text":"Hello <script>"}]}]`)
	got, err := Project("s", "r", "shop.example", "en", manifest, pages)
	if err != nil || got.Products[0].UnitAmount != "9007199254740993" {
		t.Fatalf("projection: %+v %v", got, err)
	}
	for _, bad := range []string{`{"version":2}`, `{"version":1,"locale":"en","title":"X","description":"","navigation":[{"label":"X","href":"//evil.example"}],"products":[]}`, `{"version":1,"locale":"en","title":"X","description":"","navigation":[],"products":[],"private":"secret"}`} {
		if _, err := Project("s", "r", "shop.example", "en", []byte(bad), pages); err == nil {
			t.Errorf("bad manifest accepted: %s", bad)
		}
	}
	var ps []map[string]any
	_ = json.Unmarshal(pages, &ps)
	ps[0]["blocks"] = []map[string]string{{"kind": "raw_html", "text": "<script>"}}
	bad, _ := json.Marshal(ps)
	if _, err := Project("s", "r", "shop.example", "en", manifest, bad); err == nil {
		t.Fatal("raw HTML block accepted")
	}
}
