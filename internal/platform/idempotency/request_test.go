package idempotency

import (
	"errors"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestValidateKey(t *testing.T) {
	for _, key := range []string{"x", strings.Repeat("!", 128), "a,b", "A~@"} {
		got, err := ValidateKey(http.Header{"Idempotency-Key": {key}})
		if err != nil || got != key {
			t.Errorf("valid key length%d: %q %v", len(key), got, err)
		}
	}
	for name, h := range map[string]http.Header{
		"missing": {}, "empty": {"Idempotency-Key": {""}}, "space": {"Idempotency-Key": {"a b"}}, "trim": {"Idempotency-Key": {" key"}}, "oversize": {"Idempotency-Key": {strings.Repeat("x", 129)}}, "tab": {"Idempotency-Key": {"a\tb"}}, "newline": {"Idempotency-Key": {"a\nb"}}, "unicode": {"Idempotency-Key": {"é"}}, "DEL": {"Idempotency-Key": {"\x7f"}}, "multi": {"Idempotency-Key": {"a", "a"}}, "casealias": {"Idempotency-Key": {"a"}, "idempotency-key": {"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateKey(h)
			var e *apperrors.Error
			if !errors.As(err, &e) || e.Status != 400 || e.Code != "invalid_idempotency_key" {
				t.Fatalf("expected400/invalid_idempotency_key, got%v", err)
			}
		})
	}
}
func TestFingerprintAxesCanonicalAndRawBody(t *testing.T) {
	base := Fingerprint("POST", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "a"}, url.Values{"z": {"2", "1"}, "a": {"x"}}, []byte(`{"a":1}`))
	q := url.Values{"a": {"x"}, "z": {"1", "2"}}
	before := url.Values{}
	for k, v := range q {
		before[k] = append([]string(nil), v...)
	}
	equal := Fingerprint("POST", "/sites/{id}", map[string]string{"id": "a", "tenant_id": "t"}, q, []byte(`{"a":1}`))
	if base != equal || !reflect.DeepEqual(q, before) {
		t.Fatal("canonical order changed identity or mutated query")
	}
	variants := [][32]byte{
		Fingerprint("PUT", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "a"}, q, []byte(`{"a":1}`)),
		Fingerprint("POST", "/drafts/{id}", map[string]string{"tenant_id": "t", "id": "a"}, q, []byte(`{"a":1}`)),
		Fingerprint("POST", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "b"}, q, []byte(`{"a":1}`)),
		Fingerprint("POST", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "a"}, url.Values{"a": {"y"}, "z": {"1", "2"}}, []byte(`{"a":1}`)),
		Fingerprint("POST", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "a"}, q, []byte(`{ "a":1}`)),
		Fingerprint("POST", "/sites/{id}", map[string]string{"tenant_id": "t", "id": "a"}, url.Values{"a": {"x"}, "z": {"1", "2", "2"}}, []byte(`{"a":1}`)),
	}
	for i, h := range variants {
		if h == base {
			t.Errorf("axis%d omitted", i)
		}
	}
	if Fingerprint("POST", "/x", nil, nil, nil) != Fingerprint("POST", "/x", map[string]string{}, url.Values{}, []byte{}) {
		t.Fatal("empty body/map must have one identity")
	}
	if Fingerprint("A", "BC", nil, nil, nil) == Fingerprint("AB", "C", nil, nil, nil) {
		t.Fatal("ambiguous field concatenation")
	}
}
func TestFingerprintRequestRejectsMalformedQuery(t *testing.T) {
	a := httptest.NewRequest("POST", "/a?x=%61&y=2&y=1", nil)
	b := httptest.NewRequest("POST", "/a?y=1&x=a&y=2", nil)
	x, e := FingerprintRequest(a, "/a", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	y, e := FingerprintRequest(b, "/a", nil, nil)
	if e != nil || x != y {
		t.Fatalf("escaped semanticquery differs %v", e)
	}
	a.URL.RawQuery = "x=%zz"
	_, e = FingerprintRequest(a, "/a", nil, nil)
	var pe *apperrors.Error
	if !errors.As(e, &pe) || pe.Status != 400 {
		t.Fatalf("bad query ignored:%v", e)
	}
	a.URL.RawQuery = "x=ok;dropped=true"
	if _, e = FingerprintRequest(a, "/a", nil, nil); e == nil {
		t.Fatal("query semicolon error ignored")
	}
}

func TestFingerprintRequestRequiredInputs(t *testing.T) {
	req := httptest.NewRequest("POST", "/x", nil)
	for name, r := range map[string]*http.Request{"nil": nil, "missing_url": {Method: "POST"}, "missing_method": {URL: req.URL}} {
		t.Run(name, func(t *testing.T) {
			if _, err := FingerprintRequest(r, "/x", nil, nil); err == nil {
				t.Fatal("missing request identity accepted")
			}
		})
	}
	if _, err := FingerprintRequest(req, "", nil, nil); err == nil {
		t.Fatal("missing route template accepted")
	}
	if Fingerprint("POST", "/x", nil, nil, []byte("x")) == Fingerprint("POST", "/x", nil, nil, nil) {
		t.Fatal("empty body is not fingerprinted")
	}
}

// If-Match selects the optimistic-lock version; reusing a key against another
// version must not replay the earlier result.
func TestFingerprintRequestIncludesIfMatch(t *testing.T) {
	a := httptest.NewRequest("PATCH", "/x", nil)
	b := httptest.NewRequest("PATCH", "/x", nil)
	a.Header.Set("If-Match", `"1"`)
	b.Header.Set("If-Match", `"3"`)
	x, _ := FingerprintRequest(a, "/x", nil, nil)
	y, _ := FingerprintRequest(b, "/x", nil, nil)
	if x == y {
		t.Fatal("If-Match not fingerprinted")
	}
	c := httptest.NewRequest("PATCH", "/x", nil)
	c.Header.Set("Authorization", "Bearer other")
	z, _ := FingerprintRequest(c, "/x", nil, nil)
	if z != Fingerprint("PATCH", "/x", nil, nil, nil) {
		t.Fatal("non-semantic header changed fingerprint")
	}
}
