// Package service validates and projects the public version-1 release snapshot.
// Dependencies: publish contract + stdlib; callers: publish facade.
// Invariants: no private/unknown keys or raw HTML; local links and a bounded response.
package service

import (
	"bytes"
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

type manifest struct {
	Version     int                   `json:"version"`
	Locale      string                `json:"locale"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Navigation  []contract.Navigation `json:"navigation"`
	Products    []sealedProduct       `json:"products"`
}

// sealedProduct is the exact version-1 manifest product; live SKUs are never sealed.
type sealedProduct struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	UnitAmount  string `json:"unit_amount"`
	Currency    string `json:"currency"`
}

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,37})$`)
var currency = regexp.MustCompile(`^[A-Z]{3}$`)
// Well-formed BCP 47 subset (language[-script][-region](-variant)*) that Intl accepts,
// matched case-insensitively (BCP 47 tags are case-insensitive) on the lowercased tag;
// duplicate variants are rejected separately because Intl rejects them too.
var localePattern = regexp.MustCompile(`^[a-z]{2,3}(?:-[a-z]{4})?(?:-(?:[a-z]{2}|[0-9]{3}))?(?:-(?:[a-z0-9]{5,8}|[0-9][a-z0-9]{3}))*$`)

// canonicalLocale returns the tag in canonical case (en-US, zh-Hant-TW) or false.
func canonicalLocale(v string) (string, bool) {
	lower := strings.ToLower(v)
	if !localePattern.MatchString(lower) {
		return "", false
	}
	parts := strings.Split(lower, "-")
	seen := map[string]bool{}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		switch {
		case i == 1 && len(part) == 4 && part[0] >= 'a':
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		case len(part) == 2:
			parts[i] = strings.ToUpper(part)
		case len(part) >= 4:
			if seen[part] {
				return "", false
			}
			seen[part] = true
		}
	}
	return strings.Join(parts, "-"), true
}

func decode(raw []byte, v any) error {
	// Inputs arrive as spaced jsonb text (<= 1.5x compact, see store.preBudget); the exact
	// 1 MiB limit applies to the compact response below, so inputs get the 2 MiB bound.
	if len(raw) > 2<<20 {
		return contract.ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return contract.ErrUnavailable
	}
	if d.Decode(new(any)) != io.EOF {
		return contract.ErrUnavailable
	}
	return nil
}
func text(v string, max int, required bool) bool {
	return utf8.ValidString(v) && utf8.RuneCountInString(v) <= max && !strings.ContainsRune(v, 0) && (!required || strings.TrimSpace(v) != "")
}
func Project(siteID, releaseID, host, locale string, rawManifest, rawPages []byte) (contract.Site, error) {
	fail := func() (contract.Site, error) { return contract.Site{}, contract.ErrUnavailable }
	var m manifest
	var pages []contract.Page
	if decode(rawManifest, &m) != nil || decode(rawPages, &pages) != nil {
		return fail()
	}
	canonical, ok := canonicalLocale(m.Locale)
	if m.Version != 1 || !strings.EqualFold(m.Locale, locale) || !ok || !text(m.Title, 300, true) || !text(m.Description, 2000, false) || m.Navigation == nil || m.Products == nil || pages == nil || len(m.Navigation) > 50 || len(m.Products) > 500 || len(pages) > 100 {
		return fail()
	}
	// "/" is a valid target only when the release actually pins a root page.
	targets := map[string]bool{"/products": true}
	seenPages := map[string]bool{}
	seenProducts := map[string]bool{}
	seenIDs := map[string]bool{} // one view per product: live SKUs attach by case-insensitive ID
	for _, p := range pages {
		if !contract.SafeCMSPath(p.Path) || seenPages[p.Path] || !text(p.Title, 300, true) || !text(p.Description, 2000, false) || p.Blocks == nil || len(p.Blocks) > 200 {
			return fail()
		}
		seenPages[p.Path] = true
		targets[p.Path] = true
		for _, b := range p.Blocks {
			if (b.Kind != "heading" && b.Kind != "paragraph") || !text(b.Text, 10000, true) {
				return fail()
			}
		}
	}
	products := make([]contract.Product, 0, len(m.Products))
	for _, p := range m.Products {
		products = append(products, contract.Product{ID: p.ID, Slug: p.Slug, Name: p.Name, Description: p.Description, UnitAmount: p.UnitAmount, Currency: p.Currency, SKUs: []contract.SKU{}})
		path := "/products/" + p.Slug
		if !text(p.ID, 128, true) || !text(p.Name, 300, true) || !text(p.Description, 10000, false) || !contract.SafePath(path) || strings.Contains(p.Slug, "/") || p.Slug == "" || seenProducts[p.Slug] || seenIDs[strings.ToLower(p.ID)] || !decimal.MatchString(p.UnitAmount) || !currency.MatchString(p.Currency) {
			return fail()
		}
		seenProducts[p.Slug] = true
		seenIDs[strings.ToLower(p.ID)] = true
		targets[path] = true
	}
	for _, n := range m.Navigation {
		if !text(n.Label, 200, true) || !contract.SafePath(n.Href) || !targets[n.Href] {
			return fail()
		}
	}
	site := contract.Site{SiteID: siteID, ReleaseID: releaseID, Hostname: host, Locale: canonical, Title: m.Title, Description: m.Description, Navigation: m.Navigation, Pages: pages, Products: products}
	return withinBudget(site)
}

// withinBudget enforces the HTTP buffer limit on the complete JSON response, including Encoder's newline.
func withinBudget(site contract.Site) (contract.Site, error) {
	response, err := json.Marshal(site)
	if err != nil || len(response)+1 > 1<<20 {
		return contract.Site{}, contract.ErrUnavailable
	}
	return site, nil
}
