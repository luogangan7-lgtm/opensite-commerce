// Responsibility: pin failed staff-login initialization cleanup and error causes.
// Dependencies: private runner factory; called by the CI identity test suite.
// Invariants: fresh bounded cleanup, exactly-once close, redacted joined errors.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
)

type cleanupRunner struct {
	probeErr, closeErr error
	closes            int
	cleanup           context.Context
	cleanupErr        error
	deadline          time.Time
	bounded           bool
}

func (r *cleanupRunner) RunAuthentication(context.Context, func(context.Context, *db.Queries) error) error {
	return r.probeErr
}

func (r *cleanupRunner) Close(ctx context.Context) error {
	r.closes++
	r.cleanup, r.cleanupErr = ctx, ctx.Err()
	r.deadline, r.bounded = ctx.Deadline()
	return r.closeErr
}

func TestOpenLoginFailureCleanup(t *testing.T) {
	for _, phase := range []string{"second_open", "credential_probe", "session_probe"} {
		for _, caller := range []string{"canceled", "expired"} {
			for _, closeFails := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/close_fails_%t", phase, caller, closeFails), func(t *testing.T) {
					type key struct{}
					parent := context.WithValue(context.Background(), key{}, "cleanup-value")
					ctx, cancel := context.WithCancel(parent)
					if caller == "expired" {
						cancel()
						ctx, cancel = context.WithDeadline(parent, time.Now().Add(-time.Second))
					}
					defer cancel()
					failure := errors.New("private opening or probe detail")
					closeFailure := errors.New("private credential close detail")
					sessionCloseFailure := errors.New("private session close detail")
					credential, session := &cleanupRunner{}, &cleanupRunner{}
					if closeFails {
						credential.closeErr, session.closeErr = closeFailure, sessionCloseFailure
					}
					if phase == "credential_probe" {
						credential.probeErr = failure
					}
					if phase == "session_probe" {
						session.probeErr = failure
					}
					opens := 0
					before := time.Now()
					repo, err := openLogin(ctx, "fixture-only", time.Minute, func(context.Context, string) (authRunner, error) {
						opens++
						if opens == 1 {
							return credential, nil
						}
						cancel()
						if phase == "second_open" {
							return nil, failure
						}
						return session, nil
					})
					if repo != nil || !errors.Is(err, login.ErrDatabase) || !errors.Is(err, failure) {
						t.Errorf("FIX203_CAUSES: missing initialization cause: repo=%v err=%v", repo, err)
					}
					if err != nil && strings.Contains(err.Error(), "private") {
						t.Errorf("FIX203_REDACTION: driver details escaped: %v", err)
					}
					opened := []*cleanupRunner{credential}
					if phase != "second_open" {
						opened = append(opened, session)
					} else if session.closes != 0 {
						t.Error("closed unopened session runner")
					}
					for _, runner := range opened {
						if runner.closes != 1 || runner.cleanupErr != nil || !runner.bounded || !runner.deadline.After(before) || runner.deadline.After(time.Now().Add(2*time.Second)) {
							t.Errorf("FIX203_CLEANUP: closes=%d active_err=%v bounded=%v deadline=%v", runner.closes, runner.cleanupErr, runner.bounded, runner.deadline)
						}
						if runner.cleanup == nil {
							t.Fatal("missing cleanup context")
						}
						if runner.cleanup.Value(key{}) != "cleanup-value" || !errors.Is(runner.cleanup.Err(), context.Canceled) {
							t.Error("FIX203_CLEANUP: cleanup lost values or cancel was not released")
						}
						if closeFails && !errors.Is(err, runner.closeErr) {
							t.Error("FIX203_CAUSES: missing close cause")
						}
					}
				})
			}
		}
	}
}
