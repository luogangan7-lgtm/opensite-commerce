//go:build pgx

// Responsibilities: prove export admission binds both IDs and kind before the
// service boundary. Dependencies: native River; focused gates call these tests.
package jobs

import (
	"context"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"testing"
)

type exportSpy struct {
	effects map[string]bool
	calls   int
}

func (s *exportSpy) BuildExport(_ context.Context, t, r string) error {
	s.calls++
	s.effects[t+r] = true
	return nil
}
func TestPrivacyWorkerAdmissionAndDuplicate(t *testing.T) {
	signer, _ := NewSigner([]byte("disposable-test-signing-key-32bytes"))
	args, err := signer.customerExportArgs("11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	if err != nil {
		t.Fatal(err)
	}
	spy := &exportSpy{effects: map[string]bool{}}
	worker := &CustomerExportWorker{Signer: signer, Service: spy}
	for _, field := range []string{"tenant", "request", "kind"} {
		forged := args
		kind := CustomerDataExportKind
		switch field {
		case "tenant":
			forged.TenantID = "33333333-3333-4333-8333-333333333333"
		case "request":
			forged.RequestID = "33333333-3333-4333-8333-333333333333"
		case "kind":
			kind = "customer_unknown"
		}
		if worker.Work(context.Background(), &river.Job[CustomerDataExportArgs]{JobRow: &rivertype.JobRow{Kind: kind}, Args: forged}) == nil {
			t.Fatalf("%s admitted", field)
		}
	}
	if spy.calls != 0 {
		t.Fatal("forgery reached service")
	}
	job := &river.Job[CustomerDataExportArgs]{JobRow: &rivertype.JobRow{Kind: CustomerDataExportKind}, Args: args}
	for range 2 {
		if err := worker.Work(context.Background(), job); err != nil {
			t.Fatal(err)
		}
	}
	if spy.calls != 2 || len(spy.effects) != 1 {
		t.Fatal("duplicate service contract failed")
	}
	worker.Service = nil
	if worker.Work(context.Background(), job) == nil {
		t.Fatal("unconfigured worker admitted")
	}
}

// Exports must not land on the default queue that privacy-less workers poll
// and cancel as unknown kinds during a rolling deploy (thread T2).
func TestCustomerExportUsesDedicatedQueue(t *testing.T) {
	var args river.JobArgsWithInsertOpts = CustomerDataExportArgs{}
	if q := args.InsertOpts().Queue; q == "" || q == river.QueueDefault {
		t.Fatalf("export queue=%q", q)
	}
}
