package tenanttx

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

const goodID = "0199f072-6372-7000-8000-000000000001"

type fakeTx struct {
	steps     *[]string
	commitErr error
}

func (t *fakeTx) Commit(context.Context) error {
	*t.steps = append(*t.steps, "commit")
	return t.commitErr
}
func (t *fakeTx) Rollback(context.Context) error { *t.steps = append(*t.steps, "rollback"); return nil }

func TestRunLifecycle(t *testing.T) {
	boom := errors.New("domain failure")
	for _, tc := range []struct {
		name                                 string
		beginErr, scopeErr, fnErr, commitErr error
		want                                 []string
	}{
		{"success", nil, nil, nil, nil, []string{"begin", "scope", "fn", "commit"}},
		{"begin failure", boom, nil, nil, nil, []string{"begin"}},
		{"scope failure", nil, boom, nil, nil, []string{"begin", "scope", "rollback"}},
		{"domain failure", nil, nil, boom, nil, []string{"begin", "scope", "fn", "rollback"}},
		{"commit failure", nil, nil, nil, boom, []string{"begin", "scope", "fn", "commit", "rollback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			err := Run(context.Background(), goodID,
				func(context.Context) (*fakeTx, error) {
					got = append(got, "begin")
					if tc.beginErr != nil {
						return nil, tc.beginErr
					}
					return &fakeTx{steps: &got, commitErr: tc.commitErr}, nil
				},
				func(_ context.Context, _ *fakeTx, id string) error {
					if id != goodID {
						t.Fatal("wrong tenant")
					}
					got = append(got, "scope")
					return tc.scopeErr
				},
				func(context.Context, *fakeTx) error { got = append(got, "fn"); return tc.fnErr },
			)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("steps=%v want=%v", got, tc.want)
			}
			if (err != nil) != (tc.beginErr != nil || tc.scopeErr != nil || tc.fnErr != nil || tc.commitErr != nil) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
func TestInvalidTenantDoesNotBegin(t *testing.T) {
	for _, id := range []string{"", "tenant-A", "00000000-0000-0000-0000-000000000000", "0199F072-6372-7000-8000-000000000001", "0199f072-6372-7000-8000-00000000000x"} {
		called := false
		err := Run(context.Background(), id, func(context.Context) (*fakeTx, error) { called = true; return nil, nil },
			func(context.Context, *fakeTx, string) error { return nil }, func(context.Context, *fakeTx) error { return nil })
		if !errors.Is(err, ErrInvalidTenant) || called {
			t.Fatalf("id=%q err=%v begin=%v", id, err, called)
		}
	}
}
func TestCanceledBeforeBegin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := Run(ctx, goodID, func(context.Context) (*fakeTx, error) { called = true; return nil, nil },
		func(context.Context, *fakeTx, string) error { return nil }, func(context.Context, *fakeTx) error { return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v begin=%v", err, called)
	}
}
func TestPanicRollsBack(t *testing.T) {
	var steps []string
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_ = Run(context.Background(), goodID,
			func(context.Context) (*fakeTx, error) {
				steps = append(steps, "begin")
				return &fakeTx{steps: &steps}, nil
			},
			func(context.Context, *fakeTx, string) error { steps = append(steps, "scope"); return nil },
			func(context.Context, *fakeTx) error { panic("unexpected") })
	}()
	if !reflect.DeepEqual(steps, []string{"begin", "scope", "rollback"}) {
		t.Fatalf("steps=%v", steps)
	}
}
