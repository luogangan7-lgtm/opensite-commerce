// Package notifications is static application and sender composition.
// Dependencies: public contract, private service/store and caller-owned pgx.Tx.
// Called by bootstrap; owns notification deliveries/redrive requests only.
// Invariants: no connection or auth creation; recipients are frozen SQL facts.
package notifications

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/internal/service"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/notifications/internal/store"
)

type TenantRunner func(context.Context, string, func(context.Context, pgx.Tx) error) error

func New(run TenantRunner) contract.Service { return service.Admin{Repo: store.Admin{Run: run}} }

type Sender = contract.Sender
type Message = contract.Message
type Fake = service.Fake
type AmbiguousError = service.AmbiguousError

func SenderFromEnv(get func(string) string) (Sender, error) { return service.SenderFromEnv(get) }
