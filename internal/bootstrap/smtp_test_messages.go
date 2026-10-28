// Responsibility: render fake customer challenges for the isolated SMTP smoke command.
// Dependencies: existing local customer mailer and notification contract; no IO or DB.
// Invariants: fixed fake tenant/token/origin; production mailer restrictions unchanged.
package bootstrap

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
)

type smtpTestCapture struct{ messages []contract.Message }

func (s *smtpTestCapture) Send(_ context.Context, message contract.Message) error {
	s.messages = append(s.messages, message)
	return nil
}

// CustomerSMTPTestMessages renders the real templates with exclusively fake data.
func CustomerSMTPTestMessages() ([]contract.Message, error) {
	capture := &smtpTestCapture{}
	mailer, err := newCustomerMailer(capture, "https://smtp-test.invalid")
	if err != nil {
		return nil, err
	}
	for _, purpose := range []string{"register", "reset"} {
		err = mailer.Send(context.Background(), "11111111-1111-4111-8111-111111111111", "fake@safe.test", purpose, base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("f", 32))))
		if err != nil {
			return nil, err
		}
	}
	return capture.messages, nil
}
