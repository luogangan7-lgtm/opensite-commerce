// Responsibility: adapt the verified public hostname read port to inquiry admission.
// Dependencies: publish public facade and quotes identity contract; runtime startup
// installs this resolver. No SQL or caller-provided tenant/site identity is used.
// Invariants: only Request.Host selects a published site; forwarded headers are ignored.
package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish"
	pc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	qc "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
	quoteshttp "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi/quotes"
)

func quoteInquiryResolver(public *publish.Module) quoteshttp.QuotesInquiryResolver {
	return func(ctx context.Context, r *http.Request) (context.Context, error) {
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		host := r.Host
		if r.URL.Path == "/api/public/v1/inquiries" {
			host = r.Header.Get("X-Forwarded-Host")
		}
		site, err := public.Current(lookup, host)
		if err != nil {
			if errors.Is(err, pc.ErrInvalid) || errors.Is(err, pc.ErrNotFound) {
				return nil, qc.ErrNotFound
			}
			return nil, qc.ErrUnavailable
		}
		if !qc.ValidID(site.TenantID) || !qc.ValidID(site.SiteID) {
			return nil, qc.ErrUnavailable
		}
		return qc.WithTrustedInquiry(ctx, site.TenantID, site.SiteID), nil
	}
}
