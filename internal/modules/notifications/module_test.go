// Responsibility: public API admission and disabled service contract gates.
// Dependencies: module/public ports only; no transport or database mock claims.
package notifications_test

import (
	"context"
	"errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"testing"
)

func status(t *testing.T, err error, want int) {
	t.Helper()
	var app *apperrors.Error
	if !errors.As(err, &app) || app.Status != want {
		t.Fatalf("NOTIFICATION_DOMAIN_STATUS want%d got%v", want, err)
	}
}
func TestDisabledAdminAndInputAdmission(t *testing.T) {
	ctx := context.Background()
	s := notifications.New(nil)
	tenant := "11111111-1111-1111-1111-111111111111"
	id := "22222222-2222-2222-2222-222222222222"
	_, e := s.ListFailures(ctx, tenant, 100)
	status(t, e, 503)
	_, e = s.ListFailures(ctx, tenant, 101)
	status(t, e, 400)
	_, e = s.Redrive(ctx, tenant, id, "key")
	status(t, e, 503)
	for _, key := range []string{"", " key", "a b", "x\r\ny", "中文"} {
		_, e = s.Redrive(ctx, tenant, id, key)
		status(t, e, 400)
	}
}
func TestUnfencedWorkerSendFailsClosed(t *testing.T) {
	m := notifications.NewWorker(nil, &notifications.Fake{})
	if !errors.Is(m.Send(context.Background(), outbox.Delivery{}), outbox.ErrInvalidInput) {
		t.Fatal("NOTIFICATION_UNFENCED_SEND")
	}
}
