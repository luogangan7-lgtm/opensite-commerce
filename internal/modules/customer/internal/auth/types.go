// Package auth owns tenant-bound buyer password, verification and session flows.
// Dependencies: public customer contract, standard library and Argon2 IDKey; module composition
// calls New. Invariants: employee authority is never accepted and tokens are opaque.
package auth

import (
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"time"
	"golang.org/x/crypto/argon2"
)

type Config struct {
	Repo         c.Repository
	Mailer       c.Mailer
	Now          func() time.Time
	HashSlots    int
	HashTimeout  time.Duration
	SessionTTL   time.Duration
	IdleTTL      time.Duration
	ChallengeTTL time.Duration
	Secret       []byte
}

type Service struct {
	cfg       Config
	deriveKey func([]byte, []byte, uint32, uint32, uint8, uint32) []byte
	hashSlots chan struct{}
	admission chan struct{}
}

var _ c.Service = (*Service)(nil)

// New fixes resource ceilings before serving traffic. Database and delivery
// capabilities must be supplied explicitly; no fallback staff pool exists.
func New(cfg Config) (*Service, error) {
	if cfg.Repo == nil || cfg.Mailer == nil || len(cfg.Secret) < 32 || len(cfg.Secret) > 128 { return nil,c.ErrInvalidInput }
	if cfg.Now == nil { cfg.Now=time.Now }
	if cfg.HashSlots == 0 { cfg.HashSlots=2 }
	if cfg.HashTimeout == 0 { cfg.HashTimeout=5*time.Second }
	if cfg.SessionTTL == 0 { cfg.SessionTTL=12*time.Hour }
	if cfg.IdleTTL == 0 { cfg.IdleTTL=30*time.Minute }
	if cfg.ChallengeTTL == 0 { cfg.ChallengeTTL=15*time.Minute }
	if cfg.HashSlots < 1 || cfg.HashSlots > 8 || cfg.HashTimeout < time.Millisecond || cfg.HashTimeout > 30*time.Second || cfg.IdleTTL < time.Minute || cfg.IdleTTL > 24*time.Hour || cfg.SessionTTL < cfg.IdleTTL || cfg.SessionTTL > 7*24*time.Hour || cfg.ChallengeTTL < time.Minute || cfg.ChallengeTTL > time.Hour { return nil,c.ErrInvalidInput }
	cfg.Secret=append([]byte(nil),cfg.Secret...)
	return &Service{cfg:cfg,deriveKey:argon2.IDKey,hashSlots:make(chan struct{},cfg.HashSlots),admission:make(chan struct{},cfg.HashSlots)},nil
}
