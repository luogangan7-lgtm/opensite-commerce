// Package contract owns data-rights ports and metadata-only DTOs. HTTP and jobs
// depend on this package; only trusted caller transactions select subjects.
// Owned tables: data_rights_exports/data_rights_erasures. No raw tokens persist.
package contract

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

var (
	ErrInvalid     = errors.New("data rights: invalid input")
	ErrNotFound    = errors.New("data rights: not found")
	ErrConflict    = errors.New("data rights: unavailable capability")
	ErrBusy        = errors.New("data rights: concurrent operation; retry")
	ErrUnavailable = errors.New("data rights: unavailable")
)

const MaxBytes = 10 << 20

type Config struct {
	Run      func(context.Context, string, func(context.Context, pgx.Tx) error) error
	Root     string
	TokenKey []byte
}
type RequestInput struct{ ID, CustomerID, ActorKind, ActorID string }
type EraseInput struct{ CustomerID, ActorKind, ActorID string }
type Request struct {
	ID                string    `json:"id"`
	CustomerID        string    `json:"customer_id"`
	Status            string    `json:"status"`
	ExpiresAt         time.Time `json:"expires_at"`
	DownloadExpiresAt time.Time `json:"download_expires_at"`
	DownloadToken     string    `json:"download_token,omitempty"`
}
type Erasure struct {
	CustomerID string         `json:"customer_id"`
	Requests   []string       `json:"requests"`
	Counts     map[string]int `json:"counts"`
}
type Service interface {
	RequestExport(context.Context, pgx.Tx, string, RequestInput) (Request, error)
	Status(context.Context, pgx.Tx, string, string, string) (Request, error)
	BuildExport(context.Context, string, string) error
	Download(context.Context, string, string, string, string) ([]byte, error)
	Erase(context.Context, string, EraseInput) (Erasure, error)
	EraseTx(context.Context, pgx.Tx, string, EraseInput) (Erasure, error)
	Cleanup(context.Context, string, []string) error
}
