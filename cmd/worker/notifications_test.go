//go:build pgx

// Responsibility: worker configuration and dedicated notification capability gates.
// Dependencies: worker composition, notification public port; called by go test.
// Invariants: defaults off, local-only SMTP, production rejected, fenced route only.
package main

import (
	"context"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/platform/outbox"
	"testing"
)

func TestNotificationSenderConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name             string
		values           map[string]string
		enabled, wantErr bool
	}{
		{"default", nil, false, false},
		{"local", map[string]string{"NOTIFICATIONS_SMTP_ENABLED": "true", "NOTIFICATIONS_SMTP_HOST": "127.0.0.1", "NOTIFICATIONS_SMTP_PORT": "1025"}, true, false},
		{"external", map[string]string{"NOTIFICATIONS_SMTP_ENABLED": "true", "NOTIFICATIONS_SMTP_HOST": "smtp.example.com", "NOTIFICATIONS_SMTP_PORT": "25"}, false, true},
		{"production", map[string]string{"NOTIFICATIONS_SMTP_ENABLED": "true", "NOTIFICATIONS_SMTP_HOST": "127.0.0.1", "NOTIFICATIONS_SMTP_PORT": "1025", "APP_ENV": "production"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sender, err := configuredSender(func(k string) string { return tc.values[k] })
			if (err != nil) != tc.wantErr || (sender != nil) != tc.enabled {
				t.Fatalf("NOTIFICATION_WORKER_CONFIG enabled=%v err=%v", sender != nil, err)
			}
		})
	}
}

func TestNotificationDedicatedDispatchComposition(t *testing.T) {
	marker := configuredNotificationSender{sender: &notifications.Fake{}}
	store, sender := dispatchComponents(nil, marker)
	if _, ok := store.(outbox.PGStore); ok {
		t.Fatal("NOTIFICATION_DEDICATED_STORE: generic claims consume unrelated targets")
	}
	if _, ok := sender.(outbox.LeaseSender); !ok {
		t.Fatal("NOTIFICATION_FENCED_SENDER")
	}
	if err := marker.Send(context.Background(), outbox.Delivery{}); err == nil {
		t.Fatal("NOTIFICATION_UNFENCED_SEND")
	}
	generic := testSender(func(context.Context, outbox.Delivery) error { return nil })
	store, sender = dispatchComponents(nil, generic)
	if _, ok := store.(outbox.PGStore); !ok {
		t.Fatal("generic test capability changed")
	}
	if _, ok := sender.(testSender); !ok {
		t.Fatal("generic test sender changed")
	}
}
