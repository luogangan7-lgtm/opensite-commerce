// Responsibility: committed send tokens separate database admission from IO.
// Dependencies: repository/contract and platform outbox lease; bootstrap calls
// Process. Invariants: sent/uncertain never resend, fence mandatory, safe codes.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
	"io"
	"time"
)

type Envelope struct{ ID, EventID, Recipient, Locale, TemplateKey, Reference, Status, Token string }
type WorkerRepository interface {
	Begin(context.Context, outbox.Lease, string) (Envelope, error)
	Finish(context.Context, outbox.Lease, Envelope, string, string) (bool, error)
}
type Worker struct {
	Repo   WorkerRepository
	Sender contract.Sender
}

func (w Worker) Process(ctx context.Context, l outbox.Lease) error {
	if w.Repo == nil || w.Sender == nil || !tenanttx.ValidTenantID(l.Delivery.TenantID) || !tenanttx.ValidTenantID(l.Delivery.EventID) || !tenanttx.ValidTenantID(l.WorkerID) || l.Attempt < 1 || !time.Now().Before(l.LeaseUntil) || l.Delivery.Target != "notifications.email" || l.Delivery.EventType != "notification.dispatch" || len(l.Delivery.Payload) > 1024 {
		return outbox.ErrInvalidInput
	}
	var payload struct {
		NotificationID string `json:"notification_id"`
	}
	d := json.NewDecoder(bytes.NewReader(l.Delivery.Payload))
	d.DisallowUnknownFields()
	if e := d.Decode(&payload); e != nil || !tenanttx.ValidTenantID(payload.NotificationID) {
		return outbox.ErrInvalidInput
	}
	if d.Decode(new(any)) != io.EOF {
		return outbox.ErrInvalidInput
	}
	row, e := w.Repo.Begin(ctx, l, payload.NotificationID)
	if e != nil {
		return e
	}
	switch row.Status {
	case "sent", "uncertain", "dead_letter":
		return nil
	case "sending":
	default:
		return outbox.ErrLeaseLost
	}
	if row.Token == "" {
		return outbox.ErrLeaseLost
	}
	m, e := Render(row.EventID, row.Recipient, row.Locale, row.TemplateKey, row.Reference)
	if e == nil {
		e = w.Sender.Send(ctx, m)
	}
	outcome, code := "sent", ""
	if e != nil {
		outcome, code = "failed", "NOTIFICATION_SEND_FAILED"
		var ambiguous AmbiguousError
		if errors.As(e, &ambiguous) {
			outcome, code = "uncertain", "NOTIFICATION_SEND_UNCERTAIN"
		}
	}
	// Persist acceptance/uncertainty under a bounded detached context even when
	// IO used up the send budget. SQL still rejects expired/replaced token leases.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	accepted, finishErr := w.Repo.Finish(finishCtx, l, row, outcome, code)
	if finishErr != nil {
		return finishErr
	}
	if !accepted {
		return outbox.ErrLeaseLost
	}
	if outcome == "failed" {
		return errors.New("NOTIFICATION_SEND_FAILED")
	}
	return nil
}
