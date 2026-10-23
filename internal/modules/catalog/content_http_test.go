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
