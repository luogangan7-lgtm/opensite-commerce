// Responsibilities: define typed CMS commands and draft/release results.
// Dependencies: stdlib and pgx transaction callback; callers: publish facade/HTTP.
// Invariants: actor comes from trusted context; receipt completion shares the mutation transaction.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

var ErrConflict = errors.New("CMS revision conflict")
var ErrPreviewDenied = errors.New("preview grant is invalid")

const MaxWriteBytes = 1 << 20
const MaxPreviewTTL = 5 * time.Minute

type PageDocument struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Blocks      []Block `json:"blocks"`
}
type DraftPage struct {
	ID       string       `json:"id"`
	PageKey  string       `json:"page_key"`
	Locale   string       `json:"locale"`
	Path     string       `json:"path"`
	Document PageDocument `json:"document"`
	MediaIDs []string     `json:"media_ids"`
}
type Draft struct {
	DraftID             string       `json:"draft_id"`
	SiteID              string       `json:"site_id"`
	Revision            int64        `json:"revision"`
	BaseReleaseID       *string      `json:"base_release_id"`
	PublicationRevision int64        `json:"publication_revision"`
	Status              string       `json:"status"`
	Locale              string       `json:"locale"`
	Title               string       `json:"title"`
	Description         string       `json:"description"`
	Navigation          []Navigation `json:"navigation"`
	Pages               []DraftPage  `json:"pages"`
	Products            []Product    `json:"products"`
}
type Release struct {
	ReleaseID           string  `json:"release_id"`
	ReleaseNo           int64   `json:"release_no"`
	PublicationRevision int64   `json:"publication_revision"`
	RestoredFrom        *string `json:"restored_from"`
	ManifestHash        string  `json:"manifest_hash"`
	CreatedAt           string  `json:"created_at"`
}
type Validation struct {
	Valid    bool  `json:"valid"`
	Revision int64 `json:"revision"`
}
type Result struct {
	Draft      *Draft      `json:"draft,omitempty"`
	Page       *DraftPage  `json:"page,omitempty"`
	Pages      []DraftPage `json:"pages,omitempty"`
	Release    *Release    `json:"release,omitempty"`
	Releases   []Release   `json:"releases,omitempty"`
	Validation *Validation `json:"validation,omitempty"`
	// PublicationRevision is the site epoch read with Releases (history ETag).
	PublicationRevision int64 `json:"publication_revision,omitempty"`
}
type Command struct {
	Operation                                                       string
	TenantID, SiteID, DraftID, PageID, ReleaseID, ExpectedReleaseID string
	ExpectedRevision, ExpectedPublicationRevision                   int64
	Payload                                                         json.RawMessage
	ReceiptKey                                                      string
	Complete                                                        func(context.Context, pgx.Tx, Result) error
}
type PreviewClaims struct {
	Audience  string `json:"aud"`
	TenantID  string `json:"tenant"`
	SiteID    string `json:"site"`
	DraftID   string `json:"draft"`
	Revision  int64  `json:"revision"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}
type PreviewGrant struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Revision  int64     `json:"revision"`
}
type Writer interface {
	Execute(context.Context, Command) (Result, error)
	IssuePreview(context.Context, Command, time.Duration) (PreviewGrant, error)
	Preview(context.Context, string, string, string, string) (Site, error)
}
