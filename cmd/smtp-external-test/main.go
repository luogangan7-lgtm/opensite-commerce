// Responsibility: manual CI-only smoke delivery of fake existing email templates.
// Dependencies: notification test transport and bootstrap fake challenge renderer.
// Invariants: one attempt per kind, one owner-allowlisted recipient, safe summary only.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/bootstrap"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
)

func main() {
	if run(os.Getenv, os.Stdout) != nil { os.Exit(1) }
}

func run(get func(string) string, output io.Writer) error {
	// Only the owner-approved protected main workflow may use the real command.
	invalid := errors.New("SMTP_TEST_REFUSED")
	if get("GITHUB_ACTIONS") != "true" || get("GITHUB_EVENT_NAME") != "workflow_dispatch" || get("GITHUB_REF") != "refs/heads/main" || get("APP_ENV") != "test" || strings.Contains(get("SMTP_TEST_TO"), ",") { return invalid }
	sender, err := notifications.ExternalTestSenderFromEnv(get)
	if err != nil { return invalid }
	return sendTemplates(sender, get("SMTP_TEST_TO"), output)
}

func sendTemplates(sender notifications.Sender, recipient string, output io.Writer) error {
	messages, err := bootstrap.CustomerSMTPTestMessages()
	if err != nil { return errors.New("SMTP_TEST_FIXTURE_FAILED") }
	kinds := []string{"customer.verification", "customer.password_reset", "order.paid", "order.shipped", "order.refund", "quote.sent", "quote.approved", "quote.converted"}
	for i := 2; i < len(kinds); i++ {
		message, err := notifications.RenderTestNotification("SMTP-TEST-"+kinds[i], recipient, kinds[i])
		if err != nil { return errors.New("SMTP_TEST_FIXTURE_FAILED") }
		messages = append(messages, message)
	}
	failed := false
	for i, message := range messages {
		message.Recipient = recipient
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := sender.Send(ctx, message); cancel()
		status := "sent"
		if err != nil {
			failed = true; status = "failed"
			var ambiguous notifications.AmbiguousError
			if errors.As(err, &ambiguous) { status = "uncertain" }
		}
		if _, err := fmt.Fprintf(output, "kind=%s status=%s attempts=1\n", kinds[i], status); err != nil { return errors.New("SMTP_TEST_OUTPUT_FAILED") }
	}
	if failed { return errors.New("SMTP_TEST_DELIVERY_FAILED") }
	return nil
}
