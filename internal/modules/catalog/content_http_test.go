// Responsibilities: pin staff content/listing admission through the real router.
// Dependencies: catalog service and existing admission fixture; caller: CI Go gates.
// Invariant: the new private routes exist and never bypass tenant or scope checks.
package catalog

import (
	"net/http"
	"testing"
)

func TestStaffContentAdmission(t *testing.T) {
	base := "/api/v1/t/" + tenantA + "/products/" + missingID
	for _, suffix := range []string{"/content/en", "/listings/" + missingID + "/en"} {
		t.Run(suffix, func(t *testing.T) {
			r := testRouter(t, New(Config{}), testAuth{tenant: tenantA})
			rr := httpRequest(r, http.MethodGet, base+suffix, "", "")
			if rr.Code != 503 {
				t.Fatalf("staff route HTTP %d want 503: %s", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private content must be no-store")
			}
			denied := testRouter(t, New(Config{}), testAuth{tenant: tenantA, denied: true})
			if got := httpRequest(denied, "GET", base+suffix, "", "").Code; got != 403 {
				t.Fatalf("scope denied HTTP %d", got)
			}
		})
	}
}

// A final transport commit cannot acquire Cache-Control retroactively.
func TestStaffContentRejectsLateHeader(t *testing.T) {
	for _, late := range []bool{false, true} {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if !late {
				w.Header().Set("Cache-Control", "no-store")
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			if late {
				w.Header().Set("Cache-Control", "no-store")
			}
		})
		rr := httpRequest(h, http.MethodGet, "/", "", "")
		if got := rr.Header().Get("Cache-Control") == "no-store"; got == late {
			t.Fatalf("CATALOG_SENT_HEADER_RED: late=%v accepted=%v", late, got)
		}
	}
}
