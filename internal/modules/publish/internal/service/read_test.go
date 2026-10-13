// Responsibility: validate immutable manifest/page projection; caller: Go gate.
// Dependencies: service/contract; invariant: invalid snapshots cannot reach renderer.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"strings"
	"testing"
)

func TestProjectionResponseBudget(t *testing.T) {
	products := make([]contract.Product, 100)
	for i := range products {
		products[i] = contract.Product{ID: fmt.Sprintf("p%d", i), Slug: fmt.Sprintf("p%d", i), Name: "Product", Description: strings.Repeat("x", 9000), UnitAmount: "100", Currency: "USD"}
	}
	rawManifest, err := json.Marshal(manifest{Version: 1, Locale: "en", Title: "Shop", Navigation: []contract.Navigation{}, Products: products})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		pageCount   int
		unavailable bool
	}{
		{"combined response exceeds transport budget", 5, true},
		{"large response below transport budget remains available", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pages := make([]contract.Page, tc.pageCount)
			for i := range pages {
				pages[i] = contract.Page{Path: fmt.Sprintf("/page%d", i), Title: "Page", Blocks: []contract.Block{}}
				for j := 0; j < 4; j++ {
					pages[i].Blocks = append(pages[i].Blocks, contract.Block{Kind: "paragraph", Text: strings.Repeat("x", 9000)})
				}
			}
			rawPages, err := json.Marshal(pages)
			if err != nil {
				t.Fatal(err)
			}
			if len(rawManifest) >= 1<<20 || len(rawPages) >= 1<<20 {
				t.Fatal("fixture must pass both input budgets")
			}
			t.Logf("manifest=%d pages=%d", len(rawManifest), len(rawPages))
			got, err := Project("s", "r", "shop.example", "en", rawManifest, rawPages)
			if tc.unavailable {
				if !errors.Is(err, contract.ErrUnavailable) {
					t.Fatalf("oversized combined public snapshot accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("serialized response plus newline=%d", len(response)+1)
			if len(response)+1 > 1<<20 || len(response) < 900000 {
				t.Fatal("large valid snapshot must fit the transport budget")
			}
		})
	}
}

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

// Review threads rnFiO/rnFio/rnC48: character limits, root target and Intl-valid locales.
func TestProjectionReviewBoundaries(t *testing.T) {
	project := func(locale, title, nav string, pages string) error {
		manifest := []byte(`{"version":1,"locale":"` + locale + `","title":"` + title + `","description":"","navigation":` + nav + `,"products":[]}`)
		_, err := Project("s", "r", "shop.example", locale, manifest, []byte(pages))
		return err
	}
	if err := project("zh", strings.Repeat("店", 300), `[]`, `[]`); err != nil {
		t.Errorf("300-character non-ASCII title rejected by byte length: %v", err)
	}
	if err := project("zh", strings.Repeat("店", 301), `[]`, `[]`); !errors.Is(err, contract.ErrUnavailable) {
		t.Errorf("301-character title accepted: %v", err)
	}
	if err := project("en", "Shop", `[{"label":"Home","href":"/"}]`, `[]`); !errors.Is(err, contract.ErrUnavailable) {
		t.Errorf("home link without a root page accepted: %v", err)
	}
	if err := project("en", "Shop", `[{"label":"Home","href":"/"}]`, `[{"path":"/","title":"Home","description":"","blocks":[]}]`); err != nil {
		t.Errorf("home link with a root page rejected: %v", err)
	}
	for _, locale := range []string{"en", "en-US", "zh-Hant-TW", "es-419", "de-CH-1996", "sl-rozaj-biske"} {
		if err := project(locale, "Shop", `[]`, `[]`); err != nil {
			t.Errorf("valid locale %q rejected: %v", locale, err)
		}
	}
	for _, locale := range []string{"en-US-US", "en-abcd-abcd", "en-abcde-abcde", "en_US", "en-", "e"} {
		if err := project(locale, "Shop", `[]`, `[]`); !errors.Is(err, contract.ErrUnavailable) {
			t.Errorf("Intl-invalid locale %q accepted: %v", locale, err)
		}
	}
}

// Review thread rtvb1: BCP 47 tags are case-insensitive; output uses canonical case.
func TestProjectionLocaleCaseInsensitive(t *testing.T) {
	for raw, want := range map[string]string{"en-us": "en-US", "EN-US": "en-US", "en-US": "en-US", "EN": "en",
		"zh-hant-tw": "zh-Hant-TW", "ZH-HANT-TW": "zh-Hant-TW", "es-419": "es-419", "de-ch-1996": "de-CH-1996"} {
		manifest := []byte(`{"version":1,"locale":"` + raw + `","title":"Shop","description":"","navigation":[],"products":[]}`)
		got, err := Project("s", "r", "shop.example", raw, manifest, []byte(`[]`))
		if err != nil || got.Locale != want {
			t.Errorf("locale %q: got %q, %v; want %q", raw, got.Locale, err, want)
		}
	}
	for _, raw := range []string{"en-us-us", "EN-US-US", "en_us", "en-abcd-abcd", "en-", "e"} {
		manifest := []byte(`{"version":1,"locale":"` + raw + `","title":"Shop","description":"","navigation":[],"products":[]}`)
		if _, err := Project("s", "r", "shop.example", raw, manifest, []byte(`[]`)); !errors.Is(err, contract.ErrUnavailable) {
			t.Errorf("invalid locale %q accepted: %v", raw, err)
		}
	}
}
