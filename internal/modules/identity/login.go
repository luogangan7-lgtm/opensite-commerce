// Identity login port wires private staff service, sqlc repository and transport.
// Dependencies: private login/store/httpapi; bootstrap calls this public port.
// Invariant: only a dedicated app_auth LOGIN can start the staff HTTP module.
package identity

import (
	"context"
	transport "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/httpapi"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/login"
	"github.com/luogangan7-lgtm/opensite-commerce/internal/modules/identity/internal/store"
	platform "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/httpapi"
	"time"
)

type LoginConfig = login.Config
type AuthorizedActor = login.Actor
type LoginModule struct {
	repository *store.LoginRepository
	handler    *transport.Handler
}

func OpenLogin(ctx context.Context, dsn string, cfg LoginConfig) (*LoginModule, error) {
	idle := cfg.IdleTimeout
	if idle == 0 {
		idle = 30 * time.Minute
	}
	repository, err := store.OpenLogin(ctx, dsn, idle)
	if err != nil {
		return nil, err
	}
	service, err := login.New(repository, cfg)
	if err != nil {
		_ = repository.Close(ctx)
		return nil, err
	}
	return &LoginModule{repository: repository, handler: transport.New(service, cfg.Origin, idle)}, nil
}
func (m *LoginModule) Routes() []platform.Route              { return m.handler.Routes() }
func (m *LoginModule) Authenticator() platform.Authenticator { return m.handler }
func (m *LoginModule) Close(ctx context.Context) error       { return m.repository.Close(ctx) }
func ActorFromContext(ctx context.Context) (AuthorizedActor, bool) {
	return login.ActorFromContext(ctx)
}
