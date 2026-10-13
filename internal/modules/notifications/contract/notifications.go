// Package contract is notifications' public application port. Transport and
// bootstrap consume it; implementations depend on pgx and stdlib send adapters.
// Invariants: tenant comes from authorization, DTOs expose no contact/body data.
package contract

import (
	"context"
	"time"
)

type Failure struct {
	ID          string    `json:"id"`
	EventID     string    `json:"event_id"`
	TemplateKey string    `json:"template_key"`
	Status      string    `json:"status"`
	Attempts    int32     `json:"attempts"`
	Generation  int32     `json:"generation"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	SafeError   string    `json:"safe_error,omitempty"`
}
type RedriveReceipt struct {
	NotificationID string `json:"notification_id"`
	Token          string `json:"token"`
	Status         string `json:"status"`
	Generation     int32  `json:"generation"`
}
type Service interface {
	ListFailures(context.Context, string, int32) ([]Failure, error)
	Redrive(context.Context, string, string, string) (RedriveReceipt, error)
}

// Message holds the frozen envelope rendered from the database, never a caller
// supplied recipient. Key stays stable across retries and manual generations.
type Message struct{ Key, Recipient, Subject, Text, HTML string }
type Sender interface {
	Send(context.Context, Message) error
}
