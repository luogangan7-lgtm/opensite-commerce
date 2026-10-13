// Application adapts authenticated context-only HTTP calls to caller-owned tenant transactions.
// Dependencies: pgx transaction callback, public quote contract/module; callers: bootstrap and HTTP.
// Invariants: runner never receives an untrusted tenant; public inquiry host/site identity is sealed.
package quotes

import (
	"context"
	"github.com/jackc/pgx/v5"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/quotes/contract"
)

type TenantRunner interface {
	InTenant(context.Context, string, func(context.Context, pgx.Tx) error) error
}
type Application struct {
	runner TenantRunner
	module *Module
}

func NewApplication(runner TenantRunner) *Application {
	return &Application{runner: runner, module: New()}
}
func NewApplicationWithModule(runner TenantRunner, module *Module) *Application {
	if module == nil {
		module = New()
	}
	return &Application{runner: runner, module: module}
}
func (a *Application) SubmitInquiry(ctx context.Context, in c.InquiryInput) (out c.Inquiry, err error) {
	i, err := c.InquiryFromContext(ctx)
	if err != nil {
		return out, err
	}
	if in.SiteID != "" && in.SiteID != i.SiteID {
		return out, c.ErrForbidden
	}
	in.SiteID = i.SiteID
	if a.runner == nil {
		return out, c.ErrUnavailable
	}
	err = a.runner.InTenant(ctx, i.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		out, err = a.module.SubmitInquiry(ctx, tx, i.TenantID, in)
		return err
	})
	return out, err
}
func (a *Application) Create(ctx context.Context, in c.CreateInput, o c.WriteOptions) (out c.Quote, err error) {
	actor, err := c.ActorFromContext(ctx)
	if err != nil {
		return out, err
	}
	if a.runner == nil {
		return out, c.ErrUnavailable
	}
	err = a.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		out, err = a.module.Create(ctx, tx, actor, in, o)
		return err
	})
	return out, err
}
func (a *Application) Get(ctx context.Context, id string) (out c.Quote, err error) {
	actor, err := c.ActorFromContext(ctx)
	if err != nil {
		return out, err
	}
	if a.runner == nil {
		return out, c.ErrUnavailable
	}
	err = a.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error { out, err = a.module.Get(ctx, tx, actor, id); return err })
	return out, err
}
func (a *Application) Transition(ctx context.Context, id string, action c.Action, o c.WriteOptions) (out c.Quote, err error) {
	actor, err := c.ActorFromContext(ctx)
	if err != nil {
		return out, err
	}
	if a.runner == nil {
		return out, c.ErrUnavailable
	}
	err = a.runner.InTenant(ctx, actor.TenantID(), func(ctx context.Context, tx pgx.Tx) error {
		out, err = a.module.Transition(ctx, tx, actor, id, action, o)
		return err
	})
	return out, err
}
