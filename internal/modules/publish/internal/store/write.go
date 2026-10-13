// Responsibilities: execute sqlc controlled CMS routines within scoped transactions.
// Dependencies: generated db, publish service/contract and trusted platform stamps.
// Callers: publish write facade; invariants: no raw SQL, actor stamped per TX;
// publish/restore also write the storefront serving row in the same transaction;
// mutation, validation and HTTP receipt completion commit or roll back together.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/publish/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
)

type Transaction func(context.Context, string, func(context.Context, pgx.Tx) error) error
type PreviewTransaction func(context.Context, contract.PreviewClaims, func(context.Context, pgx.Tx) error) error
type WriteStore struct {
	run, read Transaction
	preview   PreviewTransaction
}

func NewWriteStore(run, read Transaction, preview PreviewTransaction) *WriteStore {
	return &WriteStore{run: run, read: read, preview: preview}
}
func cmsID(id string) pgtype.UUID {
	var v pgtype.UUID
	if id != "" {
		_ = v.Scan(id)
	}
	return v
}
func cmsRevision(revision int64) pgtype.Int8 {
	return pgtype.Int8{Int64: revision, Valid: revision > 0}
}
func cmsError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "P0002":
			return contract.ErrNotFound
		case "40001", "55000", "23505":
			return contract.ErrConflict
		case "22023", "22P02", "23514", "23503":
			return contract.ErrInvalid
		case "42501":
			return contract.ErrNotFound
		}
	}
	// A deliberate receipt completion error must keep its transport classification.
	return err
}
func decodeCMS(raw []byte, v any) error {
	if len(raw) > 2<<20 {
		return contract.ErrUnavailable
	}
	// SQL jsonb serializes with spaces; use Project's 2MiB ingress budget then
	// enforce compact outgoing JSON size independently of PostgreSQL whitespace.
	if err := service.DecodeSnapshot(raw, v); err != nil {
		return contract.ErrUnavailable
	}
	b, err := json.Marshal(v)
	if err != nil || len(b)+1 > contract.MaxWriteBytes {
		return contract.ErrUnavailable
	}
	return nil
}
func readCMSDraft(ctx context.Context, q *db.Queries, c contract.Command) (contract.Draft, error) {
	raw, err := q.CMSReadDraft(ctx, db.CMSReadDraftParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID), DraftID: cmsID(c.DraftID), ExpectedRevision: cmsRevision(c.ExpectedRevision)})
	if err != nil {
		return contract.Draft{}, err
	}
	var d contract.Draft
	if err = decodeCMS(raw, &d); err != nil {
		return d, err
	}
	if d.DraftID != c.DraftID || d.SiteID != c.SiteID || d.Revision < 1 {
		return d, contract.ErrUnavailable
	}
	return d, nil
}
func (s *WriteStore) Execute(ctx context.Context, c contract.Command, actor httpapi.Actor) (result contract.Result, err error) {
	runner := s.run
	if c.Complete == nil && (c.Operation == "get_draft" || c.Operation == "list_pages" || c.Operation == "get_page" || c.Operation == "list_releases" || c.Operation == "get_release") {
		runner = s.read
	}
	if runner == nil {
		return result, contract.ErrUnavailable
	}
	err = runner(ctx, c.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tenant, e := httpapi.AuthorizedTenant(ctx)
		if e != nil || tenant != c.TenantID {
			return contract.ErrNotFound
		}
		current, e := httpapi.AuthorizedActor(ctx)
		if e != nil {
			return e
		}
		if current != actor {
			return contract.ErrNotFound
		}
		q := db.New(tx)
		isRead := c.Operation == "get_draft" || c.Operation == "list_pages" || c.Operation == "get_page" || c.Operation == "list_releases" || c.Operation == "get_release" || c.Operation == "validate_draft"
		if !isRead {
			stamp, e := q.SetCMSActor(ctx, db.SetCMSActorParams{ActorKind: actor.Kind, ActorID: actor.ID})
			if e != nil {
				return e
			}
			if stamp.ActorKind != actor.Kind || stamp.ActorID != actor.ID {
				return contract.ErrNotFound
			}
		}
		var raw []byte
		switch c.Operation {
		case "create_draft":
			var v service.Metadata
			if e = service.StrictDecode(c.Payload, &v); e != nil {
				return e
			}
			raw, e = q.CMSCreateDraft(ctx, db.CMSCreateDraftParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID), ActorKind: actor.Kind, ActorID: cmsID(actor.ID), Locale: v.Locale, Title: v.Title, Description: v.Description})
		case "save_page", "update_page", "delete_page", "set_metadata", "set_navigation", "apply_operations":
			raw, e = q.CMSApply(ctx, db.CMSApplyParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID), DraftID: cmsID(c.DraftID), ActorKind: actor.Kind, ActorID: cmsID(actor.ID), ExpectedRevision: c.ExpectedRevision, Operation: c.Operation, Payload: c.Payload})
		case "publish_draft":
			d, e := readCMSDraft(ctx, q, c)
			if e != nil {
				return e
			}
			if e = validateDraft(d); e != nil {
				return e
			}
			var v service.PublishPayload
			if e = service.StrictDecode(c.Payload, &v); e != nil {
				return e
			}
			raw, e = q.CMSPublish(ctx, db.CMSPublishParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID), DraftID: cmsID(c.DraftID), ActorKind: actor.Kind, ActorID: cmsID(actor.ID), ExpectedRevision: c.ExpectedRevision, ExpectedReleaseID: cmsID(c.ExpectedReleaseID), ExpectedPublicationRevision: c.ExpectedPublicationRevision, RendererBuildID: v.RendererBuildID})
			if e != nil {
				return e
			}
			// CMSPublish re-reads the catalog under READ COMMITTED, so the draft
			// validated above may differ from what was sealed. Validate the sealed
			// release itself in this transaction; failure rolls everything back.
			var r contract.Release
			if e = decodeCMS(raw, &r); e != nil {
				return e
			}
			// The serving build re-projects the sealed release (validation) and writes the
			// public row in this transaction; failure rolls the whole publish back.
			if e = buildServing(ctx, q, c.TenantID, c.SiteID, r.ReleaseID, servingPublish); e != nil {
				return e // this case shadows e; return explicitly
			}
		case "restore_release":
			raw, e = q.CMSRestore(ctx, db.CMSRestoreParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID), ActorKind: actor.Kind, ActorID: cmsID(actor.ID), ExpectedReleaseID: cmsID(c.ExpectedReleaseID), ExpectedPublicationRevision: c.ExpectedPublicationRevision, ReleaseID: cmsID(c.ReleaseID)})
			if e != nil {
				return e
			}
			// Legacy releases may satisfy SQL shape checks but fail the public
			// renderer (for example duplicate locale variants); the serving build
			// re-projects and rebuilds from the release's sealed SKU membership.
			e = buildServing(ctx, q, c.TenantID, c.SiteID, c.ReleaseID, servingRestore)
		case "list_releases", "get_release":
			raw, e = q.CMSReleaseHistory(ctx, db.CMSReleaseHistoryParams{TenantID: cmsID(c.TenantID), SiteID: cmsID(c.SiteID)})
			if e != nil {
				return e
			}
			var h struct {
				SiteID              string             `json:"site_id"`
				PublicationRevision int64              `json:"publication_revision"`
				Releases            []contract.Release `json:"releases"`
			}
			if e = decodeCMS(raw, &h); e != nil {
				return e
			}
			if h.SiteID != c.SiteID || h.Releases == nil {
				return contract.ErrUnavailable
			}
			if c.Operation == "list_releases" {
				result.Releases = h.Releases
				result.PublicationRevision = h.PublicationRevision
			} else {
				for i := range h.Releases {
					if h.Releases[i].ReleaseID == c.ReleaseID {
						r := h.Releases[i]
						result.Release = &r
						break
					}
				}
				if result.Release == nil {
					return contract.ErrNotFound
				}
			}
		default:
			d, e := readCMSDraft(ctx, q, c)
			if e != nil {
				return e
			}
			switch c.Operation {
			case "get_draft":
				result.Draft = &d
			case "list_pages":
				result.Draft = &d
				result.Pages = d.Pages
			case "get_page":
				result.Draft = &d
				for i := range d.Pages {
					if d.Pages[i].ID == c.PageID {
						p := d.Pages[i]
						result.Page = &p
						break
					}
				}
				if result.Page == nil {
					return contract.ErrNotFound
				}
			case "validate_draft":
				if e = validateDraft(d); e != nil {
					return e
				}
				result.Validation = &contract.Validation{Valid: true, Revision: d.Revision}
			default:
				return contract.ErrInvalid
			}
		}
		if e != nil {
			return e
		}
		if raw != nil && (c.Operation == "publish_draft" || c.Operation == "restore_release") {
			var r contract.Release
			if e = decodeCMS(raw, &r); e != nil {
				return e
			}
			if r.ReleaseID == "" || r.PublicationRevision != c.ExpectedPublicationRevision+1 {
				return contract.ErrUnavailable
			}
			result.Release = &r
		} else if raw != nil && c.Operation != "list_releases" && c.Operation != "get_release" {
			var d contract.Draft
			if e = decodeCMS(raw, &d); e != nil {
				return e
			}
			if d.SiteID != c.SiteID || d.DraftID == "" || d.Revision < 1 {
				return contract.ErrUnavailable
			}
			result.Draft = &d
			if c.Operation == "save_page" || c.Operation == "update_page" {
				var saved service.SavePage
				if e = service.StrictDecode(c.Payload, &saved); e != nil {
					return e
				}
				for i := range d.Pages {
					if d.Pages[i].PageKey == saved.PageKey && d.Pages[i].Locale == saved.Locale {
						p := d.Pages[i]
						result.Page = &p
						break
					}
				}
				if result.Page == nil {
					return contract.ErrUnavailable
				}
			}
		}
		if c.Complete != nil {
			return c.Complete(ctx, tx, result)
		}
		return nil
	})
	if err != nil {
		return contract.Result{}, cmsError(err)
	}
	return result, nil
}

// projectSealed renders the just-sealed or restored release exactly as the public
// reader will (longest hostname budget) inside the mutating transaction, so an
// unrenderable release never commits its pointer, audit or receipt.
func projectSealed(ctx context.Context, q *db.Queries, tenant, site, release string) (contract.Site, error) {
	snapshot, err := q.LoadCMSReleaseSnapshot(ctx, db.LoadCMSReleaseSnapshotParams{TenantID: cmsID(tenant), SiteID: cmsID(site), ReleaseID: cmsID(release)})
	if err != nil {
		return contract.Site{}, err
	}
	if snapshot.SiteID != site || snapshot.ReleaseID != release {
		return contract.Site{}, contract.ErrNotFound
	}
	view, err := service.Project(snapshot.SiteID, snapshot.ReleaseID, service.BudgetHost, snapshot.Locale, snapshot.Manifest, snapshot.Pages)
	if err != nil {
		return contract.Site{}, contract.ErrInvalid
	}
	return view, nil
}
func validateDraft(d contract.Draft) error {
	if d.Status != "open" {
		return contract.ErrConflict
	}
	root := false
	for _, p := range d.Pages {
		if p.Path == "/" {
			root = true
			break
		}
	}
	if !root {
		return contract.ErrInvalid
	}
	if _, err := service.DraftSite(d, service.BudgetHost); err != nil {
		return contract.ErrInvalid
	}
	return nil
}
func (s *WriteStore) Preview(ctx context.Context, claims contract.PreviewClaims) (d contract.Draft, err error) {
	if s.preview == nil {
		return d, contract.ErrUnavailable
	}
	err = s.preview(ctx, claims, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = readCMSDraft(ctx, db.New(tx), contract.Command{TenantID: claims.TenantID, SiteID: claims.SiteID, DraftID: claims.DraftID, ExpectedRevision: claims.Revision})
		return e
	})
	err = cmsError(err)
	if errors.Is(err, contract.ErrConflict) {
		err = contract.ErrNotFound
	}
	return d, err
}
