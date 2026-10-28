// Responsibility: owner password reauthentication without rotating global sessions.
// Dependencies: shared login admission/KDF and private Identity proof repository.
// Callers: the explicitly composed merchant step-up transport; no route activation.
// Invariants: opaque digest only, session/user/tenant/action bound, DB expiry.
package login

import (
	"context"
	"net/netip"
	"time"

	"github.com/luogangan7-lgtm/opensite-commerce/internal/tenanttx"
)

type merchantRepository interface {
	MerchantIdentifier(context.Context, string) (string, error)
	IssueMerchantProof(context.Context, string, Actor, string, [32]byte, string) (time.Time, error)
	MerchantReceipt(context.Context, string, string, Actor) (bool, error)
}

type MerchantProof struct {
	Proof     string    `json:"proof"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) MerchantStepUp(ctx context.Context, actor Actor, tenant, action, password, device string, ip netip.Addr) (MerchantProof, error) {
	if !tenanttx.ValidTenantID(tenant) || actor.userID == "" || actor.sessionID == "" || (action != "request" && action != "cancel") {
		return MerchantProof{}, ErrCredentials
	}
	repo, ok := s.repo.(merchantRepository)
	if !ok {
		return MerchantProof{}, ErrDatabase
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.HashTimeout)
	defer cancel()
	identifier, err := repo.MerchantIdentifier(ctx, actor.userID)
	if err != nil {
		return MerchantProof{}, ErrCredentials
	}
	admitted, err := s.authenticatePassword(ctx, LoginInput{Identifier: identifier, Password: password, DeviceToken: device, IP: ip})
	if err != nil {
		return MerchantProof{}, err
	}
	if admitted.credential.UserID != actor.userID {
		return MerchantProof{}, ErrCredentials
	}
	raw, err := newToken()
	if err != nil {
		return MerchantProof{}, ErrDatabase
	}
	digest, _ := tokenHash(raw)
	expires, err := repo.IssueMerchantProof(ctx, tenant, actor, action, digest, admitted.credential.PasswordHash)
	if err != nil {
		return MerchantProof{}, err
	}
	return MerchantProof{Proof: raw, ExpiresAt: expires}, nil
}

func (s *Service) MerchantReceipt(ctx context.Context, tenant, request string, actor Actor) (bool, error) {
	repo, ok := s.repo.(merchantRepository)
	if !ok || !tenanttx.ValidTenantID(tenant) || !tenanttx.ValidTenantID(request) {
		return false, ErrCredentials
	}
	return repo.MerchantReceipt(ctx, tenant, request, actor)
}
