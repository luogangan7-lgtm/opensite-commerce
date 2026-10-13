// Responsibilities: customer authentication command boundary (tests-first stub).
// Dependencies: customer contract; HTTP/module callers invoke these operations.
// Invariant: no credentials, customer sessions or mailbox claims admitted yet.
package auth

import (
	"context"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"net/netip"
)

func (*Service) Register(context.Context, string, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (*Service) Verify(context.Context, string, string) error { return c.ErrUnavailable }
func (*Service) RequestReset(context.Context, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (*Service) Reset(context.Context, string, string, string, netip.Addr) error {
	return c.ErrUnavailable
}
func (*Service) Login(context.Context, string, string, string, netip.Addr, string) (c.Session, error) {
	return c.Session{}, c.ErrUnavailable
}
func (*Service) Current(context.Context, string, string) (c.Session, error) {
	return c.Session{}, c.ErrUnavailable
}
func (*Service) Logout(context.Context, string, string) error { return c.ErrUnavailable }
