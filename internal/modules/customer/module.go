// Package customer composes the public customer-authentication service.
// Dependencies: own contract/auth/store and generated db queries; bootstrap supplies
// authorized customer-auth transactions and a Mailer. No connection or credential
// is created here; customer sessions can never confer staff authority.
package customer

import (
	"context"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/db"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/internal/auth"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/internal/store"
)

type Config struct {
	Repo         contract.Repository
	Mailer       contract.Mailer
	Secret       []byte
	Now          func() time.Time
	HashSlots    int
	HashTimeout  time.Duration
	SessionTTL   time.Duration
	IdleTTL      time.Duration
	ChallengeTTL time.Duration
}

func New(config Config) (contract.Service, error) {
	return auth.New(auth.Config{Repo: config.Repo, Mailer: config.Mailer, Secret: config.Secret, Now: config.Now, HashSlots: config.HashSlots, HashTimeout: config.HashTimeout, SessionTTL: config.SessionTTL, IdleTTL: config.IdleTTL, ChallengeTTL: config.ChallengeTTL})
}

// TenantQueries is supplied by bootstrap's dedicated app_customer_auth runner.
type TenantQueries func(context.Context, string, func(*db.Queries) error) error

func NewRepository(run TenantQueries) contract.Repository { return store.New(store.Run(run)) }

func NewPostgres(run TenantQueries, config Config) (contract.Service, error) {
	config.Repo = NewRepository(run)
	return New(config)
}
