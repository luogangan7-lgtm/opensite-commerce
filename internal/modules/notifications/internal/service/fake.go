// Responsibility: test-only concurrency-safe sender with stable-key dedup.
// Dependencies: stdlib and contract; used by local acceptance composition.
// Invariants: cancellation and failures do not consume a key; snapshots copy.
package service

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"sync"
)

type Fake struct {
	mu        sync.Mutex
	messages  map[string]contract.Message
	failures  int
	ambiguous bool
}

func (f *Fake) FailNext(count int) { f.mu.Lock(); defer f.mu.Unlock(); f.failures = count }
func (f *Fake) AmbiguousNext()     { f.mu.Lock(); defer f.mu.Unlock(); f.ambiguous = true }
func (f *Fake) Send(ctx context.Context, m contract.Message) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	if f.messages == nil {
		f.messages = map[string]contract.Message{}
	}
	if _, ok := f.messages[m.Key]; ok {
		return nil
	}
	if f.failures > 0 {
		f.failures--
		return errors.New("NOTIFICATION_SEND_FAILED")
	}
	f.messages[m.Key] = m
	if f.ambiguous {
		f.ambiguous = false
		return AmbiguousError{}
	}
	return nil
}
func (f *Fake) Snapshot() []contract.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := make([]contract.Message, 0, len(f.messages))
	for _, m := range f.messages {
		v = append(v, m)
	}
	return v
}

// AmbiguousError means DATA may have been accepted; never automatically resend.
type AmbiguousError struct{}

func (AmbiguousError) Error() string { return "NOTIFICATION_SEND_UNCERTAIN" }
