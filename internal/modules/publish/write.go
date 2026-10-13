// Responsibilities: expose authenticated CMS writes and signed readonly previews.
// Dependencies: private service/store, central auth stamp and scoped transaction ports.
// Callers: API composition/transport; invariants: auth and validation precede SQL;
// mutation and durable HTTP receipt complete in one transaction.
package publish

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/store"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"time"
)

type WriteConfig struct {
	Run     func(context.Context, string, func(context.Context, pgx.Tx) error) error
	ReadRun func(context.Context, string, func(context.Context, pgx.Tx) error) error
	// PreviewRun accepts only claims verified by this module. Bootstrap binds a
	// read-only tenant snapshot runner; route IDs alone never reach this port.
	PreviewRun func(context.Context, contract.PreviewClaims, func(context.Context, pgx.Tx) error) error
	PreviewKey []byte
	Now        func() time.Time
}
type WriteModule struct {
	store  *store.WriteStore
	signer *service.PreviewSigner
}

func NewWriter(c WriteConfig) (*WriteModule, error) {
	signer, err := service.NewPreviewSigner(c.PreviewKey, c.Now)
	if err != nil {
		return nil, err
	}
	return &WriteModule{store: store.NewWriteStore(c.Run, c.ReadRun, c.PreviewRun), signer: signer}, nil
}
func authorizeCMS(ctx context.Context, c contract.Command) (httpapi.Actor, error) {
	tenant, err := httpapi.AuthorizedTenant(ctx)
	if err != nil {
		return httpapi.Actor{}, err
	}
	if tenant != c.TenantID {
		return httpapi.Actor{}, contract.ErrNotFound
	}
	if !tenanttx.ValidTenantID(c.TenantID) || !tenanttx.ValidTenantID(c.SiteID) {
		return httpapi.Actor{}, contract.ErrInvalid
	}
	return httpapi.AuthorizedActor(ctx)
}
func validateCommand(c contract.Command) (contract.Command, error) {
	for _, id := range []string{c.DraftID, c.PageID, c.ReleaseID, c.ExpectedReleaseID} {
		if id != "" && !tenanttx.ValidTenantID(id) {
			return c, contract.ErrInvalid
		}
	}
	switch c.Operation {
	case "create_draft":
	case "restore_release":
		if c.ReleaseID == "" || c.ExpectedPublicationRevision < 0 {
			return c, contract.ErrInvalid
		}
		return c, nil
	case "list_releases", "get_release":
		if c.Operation == "get_release" && c.ReleaseID == "" {
			return c, contract.ErrInvalid
		}
		return c, nil
	case "get_draft", "list_pages", "get_page", "validate_draft":
		if c.DraftID == "" || c.ExpectedRevision < 0 || (c.Operation == "get_page" && c.PageID == "") {
			return c, contract.ErrInvalid
		}
		return c, nil
	case "save_page", "update_page", "delete_page", "set_navigation", "set_metadata", "apply_operations", "publish_draft":
		if c.DraftID == "" || c.ExpectedRevision < 1 || c.ExpectedPublicationRevision < 0 {
			return c, contract.ErrInvalid
		}
	default:
		return c, contract.ErrInvalid
	}
	p, err := service.NormalizePayload(c.Operation, c.Payload)
	if err != nil {
		return c, err
	}
	c.Payload = p
	if c.Operation == "update_page" {
		var v service.SavePage
		if c.PageID == "" || service.StrictDecode(p, &v) != nil || v.ID == nil || *v.ID != c.PageID {
			return c, contract.ErrInvalid
		}
	}
	return c, nil
}
func (m *WriteModule) Execute(ctx context.Context, c contract.Command) (contract.Result, error) {
	actor, err := authorizeCMS(ctx, c)
	if err != nil {
		return contract.Result{}, err
	}
	c, err = validateCommand(c)
	if err != nil {
		return contract.Result{}, err
	}
	if m == nil || m.store == nil {
		return contract.Result{}, contract.ErrUnavailable
	}
	return m.store.Execute(ctx, c, actor)
}
func (m *WriteModule) IssuePreview(ctx context.Context, c contract.Command, ttl time.Duration) (contract.PreviewGrant, error) {
	if _, err := authorizeCMS(ctx, c); err != nil {
		return contract.PreviewGrant{}, err
	}
	if c.DraftID == "" || !tenanttx.ValidTenantID(c.DraftID) || c.ExpectedRevision < 1 || ttl < time.Second || ttl > contract.MaxPreviewTTL {
		return contract.PreviewGrant{}, contract.ErrInvalid
	}
	if m == nil || m.store == nil || m.signer == nil {
		return contract.PreviewGrant{}, contract.ErrUnavailable
	}
	c.Operation = "get_draft"
	c.Complete = nil
	c.Payload = nil
	r, err := m.Execute(ctx, c)
	if err != nil {
		return contract.PreviewGrant{}, err
	}
	if r.Draft == nil || r.Draft.Status != "open" {
		return contract.PreviewGrant{}, contract.ErrConflict
	}
	if _, err = service.DraftSite(*r.Draft, service.BudgetHost); err != nil {
		return contract.PreviewGrant{}, err
	}
	token, expiry, err := m.signer.Issue(c.TenantID, c.SiteID, c.DraftID, r.Draft.Revision, ttl)
	if err != nil {
		return contract.PreviewGrant{}, err
	}
	return contract.PreviewGrant{Token: token, ExpiresAt: expiry, Revision: r.Draft.Revision}, nil
}
func (m *WriteModule) Preview(ctx context.Context, tenant, site, draft, token string) (contract.Site, error) {
	if m == nil || m.signer == nil || m.store == nil {
		return contract.Site{}, contract.ErrUnavailable
	}
	claims, err := m.signer.Verify(token, tenant, site, draft)
	if err != nil {
		return contract.Site{}, err
	}
	d, err := m.store.Preview(ctx, claims)
	if err != nil {
		return contract.Site{}, err
	}
	if d.DraftID != draft || d.SiteID != site || d.Revision != claims.Revision || d.Status != "open" {
		return contract.Site{}, contract.ErrNotFound
	}
	return service.DraftSite(d, "")
}

// Compile-time transport contract, not a second implementation.
var _ contract.Writer = (*WriteModule)(nil)
