// Package outbox dispatches committed leased events using an at-least-once Sender.
// Dependencies: tenanttx UUID validation and a Store whose methods each commit an
// independent tenant-scoped transaction. Called by trusted worker composition;
// never enumerates tenants, sends while a transaction is open, or stores raw errors.
package outbox

import (
	"context"
	"errors"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

var (
	ErrInvalidInput = errors.New("OUTBOX_INVALID_INPUT")
	ErrLeaseLost    = errors.New("OUTBOX_LEASE_LOST")
)

// Delivery identity is stable across lease generations. Consumers must dedup
// TenantID+EventID+Target (or TenantID+Target+DedupKey) atomically with their side effect: a send can
// succeed followed by an acknowledgement crash. Payload is opaque JSON, not logged.
type Delivery struct {
	TenantID, EventID, Target, DedupKey, EventType string
	Payload                                        []byte
}

type Lease struct {
	Delivery   Delivery
	WorkerID   string
	Attempt    int32
	LeaseUntil time.Time
}

// Sender must honor context cancellation and support idempotent redelivery.
// Successful Send means accepted by the consumer, not exactly-once execution.
type Sender interface {
	Send(context.Context, Delivery) error
}

// Store returns only after its operation's independent transaction commits.
// Complete and Fail return false for an expired/replaced lease. They must pass
// tenant, event ID, worker ID and Attempt unmodified to SQL's fencing functions.
type Store interface {
	Claim(context.Context, string, string, int32, int32) ([]Lease, error)
	Complete(context.Context, Lease) (bool, error)
	Fail(context.Context, Lease, string, int32) (bool, error)
}

type Config struct {
	WorkerID                                        string
	LeaseSeconds, BaseRetrySeconds, MaxRetrySeconds int32
}
type Dispatcher struct {
	Store  Store
	Sender Sender
	Config Config
}
type Result struct{ Claimed, Sent, Completed, Failed int }

// Dispatch performs at most maxEvents deliveries for a caller-authorized tenant.
func (d Dispatcher) Dispatch(ctx context.Context, tenantID string, maxEvents int) (Result, error) {
	var result Result
	c := d.Config
	if d.Store == nil || d.Sender == nil || !tenanttx.ValidTenantID(tenantID) ||
		!tenanttx.ValidTenantID(c.WorkerID) || maxEvents < 1 || maxEvents > 1000 ||
		c.LeaseSeconds < 1 || c.LeaseSeconds > 3600 || c.BaseRetrySeconds < 1 ||
		c.MaxRetrySeconds < c.BaseRetrySeconds || c.MaxRetrySeconds > 86400 {
		return result, ErrInvalidInput
	}
	for range maxEvents {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		leases, err := d.Store.Claim(ctx, tenantID, c.WorkerID, 1, c.LeaseSeconds)
		if err != nil {
			return result, err
		}
		if len(leases) == 0 {
			return result, nil
		}
		if len(leases) != 1 {
			return result, ErrInvalidInput
		}
		l := leases[0]
		if l.Delivery.TenantID != tenantID || l.WorkerID != c.WorkerID || l.Attempt < 1 ||
			!tenanttx.ValidTenantID(l.Delivery.EventID) || l.LeaseUntil.IsZero() ||
			l.Delivery.Target == "" || l.Delivery.DedupKey == "" {
			return result, ErrInvalidInput
		}
		result.Claimed++
		if !time.Now().Before(l.LeaseUntil) {
			return result, ErrLeaseLost
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		deadline := time.Now().Add(time.Duration(c.LeaseSeconds) * time.Second)
		if l.LeaseUntil.Before(deadline) {
			deadline = l.LeaseUntil
		}
		sendCtx, cancel := context.WithDeadline(ctx, deadline)
		delivery := l.Delivery
		delivery.Payload = append([]byte(nil), delivery.Payload...)
		sendErr := d.Sender.Send(sendCtx, delivery)
		cancel()
		if sendErr != nil {
			retry := c.BaseRetrySeconds
			// Saturate before doubling: attempts can reach MaxInt32 without overflow.
			for attempt := int32(1); attempt < l.Attempt && retry < c.MaxRetrySeconds; attempt++ {
				if retry > c.MaxRetrySeconds/2 {
					retry = c.MaxRetrySeconds
				} else {
					retry *= 2
				}
			}
			accepted, err := d.Store.Fail(ctx, l, "OUTBOX_SEND_FAILED", retry)
			if err != nil {
				return result, err
			}
			if !accepted {
				return result, ErrLeaseLost
			}
			result.Failed++
			continue
		}
		result.Sent++
		accepted, err := d.Store.Complete(ctx, l)
		if err != nil {
			return result, err
		} // Ambiguous ack leaves takeover to the lease protocol.
		if !accepted {
			return result, ErrLeaseLost
		}
		result.Completed++
	}
	return result, nil
}
