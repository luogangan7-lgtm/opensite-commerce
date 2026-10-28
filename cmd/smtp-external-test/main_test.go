// Responsibility: offline smoke harness admission, template coverage and log privacy.
// Dependencies: fake Sender only; CI runs these without SMTP credentials or network.
// Invariants: eight real template kinds, one attempt each, no sensitive output.
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
)

type smokeCapture struct {
	messages []notifications.Message
	err      error
}

func (s *smokeCapture) Send(_ context.Context, message notifications.Message) error {
	s.messages = append(s.messages, message)
	return s.err
}

func TestSmokeTemplatesSingleAttemptAndSafeSummary(t *testing.T) {
	for _, sendErr := range []error{nil, errors.New("secret-server-canary"), notifications.AmbiguousError{}} {
		capture := &smokeCapture{err: sendErr}
		var output bytes.Buffer
		err := sendTemplates(capture, "owner-allowlist@example.com", &output)
		if (err != nil) != (sendErr != nil) {
			t.Fatal("SMOKE_FAILURE_EXIT")
		}
		if len(capture.messages) != 8 {
			t.Fatal("SMOKE_ONE_PER_TEMPLATE")
		}
		status := "sent"
		if sendErr != nil {
			status = "failed"
		}
		var uncertain notifications.AmbiguousError
		if errors.As(sendErr, &uncertain) {
			status = "uncertain"
		}
		kinds := []string{"customer.verification", "customer.password_reset", "order.paid", "order.shipped", "order.refund", "quote.sent", "quote.approved", "quote.converted"}
		var expected strings.Builder
		for i, kind := range kinds {
			expected.WriteString("kind=" + kind + " status=" + status + " attempts=1\n")
			m := capture.messages[i]
			if m.Recipient != "owner-allowlist@example.com" || len(m.Key) != 64 || m.Text == "" || m.HTML == "" || strings.Contains(m.Text, m.Recipient) {
				t.Fatal("SMOKE_FAKE_ENVELOPE")
			}
		}
		if output.String() != expected.String() {
			t.Fatal("SMOKE_SUMMARY_ALLOWLIST")
		}
		if !strings.Contains(capture.messages[0].Text, "https://smtp-test.invalid/customer/verify#") || !strings.Contains(capture.messages[1].Text, "https://smtp-test.invalid/customer/reset#") {
			t.Fatal("SMOKE_EXISTING_CUSTOMER_TEMPLATES")
		}
		if capture.messages[0].Key == capture.messages[1].Key {
			t.Fatal("SMOKE_DISTINCT_KEYS")
		}
	}
}

func TestSmokeRefusesNonMainAndNonManualWithoutSending(t *testing.T) {
	for _, change := range []map[string]string{{"GITHUB_ACTIONS": ""}, {"GITHUB_EVENT_NAME": "push"}, {"GITHUB_REF": "refs/heads/test"}, {"APP_ENV": "production"}, {"SMTP_TEST_TO": "one@example.com,two@example.com"}} {
		env := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "APP_ENV": "test"}
		for k, v := range change {
			env[k] = v
		}
		var output bytes.Buffer
		if run(func(k string) string { return env[k] }, &output) == nil || output.Len() != 0 {
			t.Fatal("SMOKE_GUARD_BEFORE_NETWORK")
		}
	}
}
