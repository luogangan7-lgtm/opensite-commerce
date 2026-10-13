// Responsibility: adapt customer email challenges to the public notification sender.
// Dependencies: notifications/contract only; customer authentication supplies tokens.
// Invariants: local test recipients only, pinned HTTPS origin, token only in fragment;
// no transport errors or challenge secrets escape through returned errors.
package auth

import (
	"context"
	"errors"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

// Mailer sends an already-created single-use email challenge, never credentials.
type Mailer interface {
	Send(context.Context, string, string, string, string) error
}

type NotificationMailer struct {
	sender contract.Sender
	origin string
}

// NewNotificationMailer pins the storefront origin separately from request input.
func NewNotificationMailer(sender contract.Sender, origin string) (*NotificationMailer, error) {
	return &NotificationMailer{sender: sender, origin: origin}, nil
}

// Send is intentionally incomplete until the GitHub CI red-test witness is recorded.
func (m *NotificationMailer) Send(context.Context, string, string, string, string) error {
	return errors.New("CUSTOMER_MAILER_UNIMPLEMENTED")
}
