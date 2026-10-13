// Responsibilities: guard strict text-only commands and signed preview boundaries.
// Dependencies: publish service and contract; callers: focused Go gates.
// Invariants: unknown/raw HTML inputs fail, preview forgery never reaches storage.
package service

import (
	"encoding/json"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"strings"
	"testing"
	"time"
)

func TestCMSUpdatePageRequiresIdentity(t *testing.T) {

	valid := []byte(`{"id":"44444444-4444-4444-4444-444444444444","page_key":"home","locale":"en","path":"/","document":{"title":"Home","description":"","blocks":[]},"media_ids":[]}`)
	if _, err := NormalizePayload("update_page", valid); err != nil {
		t.Fatal(err)
	}
	missing := []byte(strings.Replace(string(valid), `"id":"44444444-4444-4444-4444-444444444444",`, "", 1))
	if _, err := NormalizePayload("update_page", missing); err == nil {
		t.Fatal("update without identity admitted")
	}
}

func TestNormalizeCMSPayload(t *testing.T) {
	valid := []byte(`{"page_key":"home","locale":"en-US","path":"/","document":{"title":"Home","description":"","blocks":[{"kind":"heading","text":"Welcome"}]},"media_ids":[]}`)
	got, err := NormalizePayload("save_page", valid)
	if err != nil || !json.Valid(got) {
		t.Fatalf("valid=%s %v", got, err)
	}
	for _, raw := range []string{
		strings.Replace(string(valid), `"Welcome"`, `"<script>alert(1)</script>"`, 1),
		strings.Replace(string(valid), `"heading"`, `"html"`, 1),
		strings.Replace(string(valid), `"page_key"`, `"Page_Key"`, 1),
		strings.Replace(string(valid), `"title":"Home"`, `"title":"Home","Title":"Other"`, 1),
		strings.Replace(string(valid), `"path":"/"`, `"path":"//evil"`, 1),
		strings.Replace(string(valid), `"description":""`, `"description":"","raw_html":"x"`, 1),
		strings.Replace(string(valid), `"page_key":"home"`, `"page_key":"home","page_key":"other"`, 1),
		string(valid) + `{}`, string([]byte{0xff}),
	} {
		if _, err := NormalizePayload("save_page", []byte(raw)); err == nil {
			t.Fatalf("accepted invalid %s", raw)
		}
	}
	if _, err := NormalizePayload("unknown", valid); err == nil {
		t.Fatal("unknown op")
	}
	if _, err := NormalizePayload("save_page", []byte(strings.Repeat(" ", (1<<20)+1))); err == nil {
		t.Fatal("oversize")
	}
}

func TestPreviewTokenBoundaries(t *testing.T) {
	now := time.Unix(1800000000, 0)
	signer, err := NewPreviewSigner([]byte(strings.Repeat("k", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tenant := "11111111-1111-4111-8111-111111111111"
	site := "22222222-2222-4222-8222-222222222222"
	draft := "33333333-3333-4333-8333-333333333333"
	token, expires, err := signer.Issue(tenant, site, draft, 7, 5*time.Minute)
	if err != nil || !expires.Equal(now.Add(5*time.Minute)) {
		t.Fatal(expires, err)
	}
	claims, err := signer.Verify(token, tenant, site, draft)
	if err != nil || claims.Revision != 7 {
		t.Fatal(claims, err)
	}
	parts := strings.Split(token, ".")
	sig := []byte(parts[1])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	wrongMAC := parts[0] + "." + string(sig)
	for _, bad := range []string{wrongMAC, token + "x", "invalid", strings.Replace(token, ".", "x", 1), strings.Repeat("x", 4097)} {
		if _, err := signer.Verify(bad, tenant, site, draft); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if _, err := signer.Verify(token, site, site, draft); err == nil {
		t.Fatal("wrong tenant")
	}
	if _, _, err := signer.Issue(tenant, site, draft, 7, 6*time.Minute); err == nil {
		t.Fatal("overlong TTL")
	}
	now = expires
	if _, err := signer.Verify(token, tenant, site, draft); err == nil {
		t.Fatal("expired token")
	}
	now = now.Add(-10 * time.Minute)
	if _, err := signer.Verify(token, tenant, site, draft); err == nil {
		t.Fatal("issued in future")
	}
	if _, err := NewPreviewSigner([]byte("short"), time.Now); err == nil {
		t.Fatal("weak key")
	}
}

func TestHTMLInputFlattensSafely(t *testing.T) {
	raw := []byte(`{"page_key":"home","locale":"EN-us","path":"/","document":{"title":"<strong>Home</strong>","description":"<p>Safe</p><style>secret</style>","blocks":[{"kind":"paragraph","text":"<p>Hello <em>world</em></p><script>alert(1)</script><iframe>hidden</iframe><svg><text>hidden</text></svg><p>Next<br>line</p>"}]},"media_ids":[]}`)
	got, err := NormalizePayload("save_page", raw)
	if err != nil {
		t.Fatal(err)
	}
	var page SavePage
	if err := json.Unmarshal(got, &page); err != nil {
		t.Fatal(err)
	}
	if page.Locale != "en-US" || page.Document.Title != "Home" || page.Document.Description != "Safe" || page.Document.Blocks[0].Text != "Hello world\nNext\nline" {
		t.Fatalf("unsafe/incorrect canonical page %+v", page)
	}
	for _, html := range []string{`<img src=x onerror=alert(1)>Hello`, `<object><p>hidden</p></object>Hello`, `<math><mtext>hidden</mtext></math>Hello`, `<template><p>hidden</p></template>Hello`, `<a href="javascript:alert(1)" onclick="x">Hello</a>`} {
		if got := cleanText(html); got != "Hello" {
			t.Errorf("%q -> %q", html, got)
		}
	}
	invalidUTF := append([]byte(`{"title":"`), 0xff)
	invalidUTF = append(invalidUTF, []byte(`","description":"","locale":"en"}`)...)
	if _, err := NormalizePayload("create_draft", invalidUTF); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
}

func TestCMSNavigationBatchAndRenderingAgreement(t *testing.T) {
	for _, raw := range []string{`{"navigation":[{"label":"External","href":"https://evil.test"}]}`, `{"navigation":null}`, `{"navigation":[{"label":"","href":"/"}]}`} {
		if _, err := NormalizePayload("set_navigation", []byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{`{"operations":[]}`, `{"operations":[{"operation":"publish_draft","payload":{}}]}`, `{"operations":[{"operation":"set_navigation","payload":{"navigation":[],"private":1}}]}`} {
		if _, err := NormalizePayload("apply_operations", []byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	d := contract.Draft{DraftID: "33333333-3333-4333-8333-333333333333", SiteID: "22222222-2222-4222-8222-222222222222", Locale: "en-US", Title: "Site", Navigation: []contract.Navigation{}, Products: []contract.Product{}, Pages: []contract.DraftPage{{ID: "44444444-4444-4444-8444-444444444444", Locale: "en-US", Path: "/", Document: contract.PageDocument{Title: "Home", Blocks: []contract.Block{{Kind: "heading", Text: "Home"}}}}}}
	if _, err := DraftSite(d); err != nil {
		t.Fatal(err)
	}
	d.Navigation = []contract.Navigation{{Label: "Missing", Href: "/absent"}}
	if _, err := DraftSite(d); err == nil {
		t.Fatal("dangling navigation render accepted")
	}
	d.Navigation = []contract.Navigation{}
	d.Pages[0].Path = "/products"
	if _, err := DraftSite(d); err == nil {
		t.Fatal("reserved route render accepted")
	}
}
