// Responsibility: observable fake send semantics gate; stdlib only.
package service

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"sync"
	"testing"
)

func TestFakeConcurrentDedupAndContext(t *testing.T) {
	f := &Fake{}
	m := contract.Message{Key: "stable", Recipient: "a@safe.test"}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if e := f.Send(context.Background(), m); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if len(f.Snapshot()) != 1 {
		t.Fatal("NOTIFICATION_FAKE_DEDUP")
	}
	s := f.Snapshot()
	s[0].Key = "changed"
	if f.Snapshot()[0].Key != "stable" {
		t.Fatal("NOTIFICATION_SNAPSHOT_COPY")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(f.Send(ctx, contract.Message{Key: "other"}), context.Canceled) {
		t.Fatal("NOTIFICATION_CONTEXT")
	}
}
func TestFakeFailureThenAmbiguousDedup(t *testing.T) {
	f := &Fake{}
	m := contract.Message{Key: "key"}
	f.FailNext(1)
	if f.Send(context.Background(), m) == nil || len(f.Snapshot()) != 0 {
		t.Fatal("NOTIFICATION_FAILURE")
	}
	f.AmbiguousNext()
	var uncertain AmbiguousError
	if !errors.As(f.Send(context.Background(), m), &uncertain) {
		t.Fatal("NOTIFICATION_AMBIGUOUS")
	}
	if e := f.Send(context.Background(), m); e != nil || len(f.Snapshot()) != 1 {
		t.Fatal("NOTIFICATION_REPLAY_DEDUP")
	}
}
