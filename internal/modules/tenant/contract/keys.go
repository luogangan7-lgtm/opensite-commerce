// Package contract owns the tenant API-key management port and safe views.
// Dependencies: context/time only; private store and public tenant module call.
// Invariants: stored hashes never enter a view; plaintext key is emitted once.
package contract

import (
	"context"
	"time"
)

type APIKeyView struct {
	ID         string     `json:"id"`
	KeyPrefix  string     `json:"key_prefix"`
	Scopes     []string   `json:"scopes"`
	IssuedBy   string     `json:"issued_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type CreateInput struct {
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type Created struct {
	Key    string     `json:"key"`
	APIKey APIKeyView `json:"api_key"`
}

type KeyMaterial struct {
	ID, Prefix string
	Hash       [32]byte
	Scopes     []string
	ExpiresAt  *time.Time
}

type Repository interface {
	List(context.Context, string, string) ([]APIKeyView, error)
	Create(context.Context, string, string, KeyMaterial) (APIKeyView, error)
	Revoke(context.Context, string, string, string) error
}
