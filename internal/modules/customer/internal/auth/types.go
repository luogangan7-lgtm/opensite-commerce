// Package auth owns tenant-bound buyer password, verification and session flows.
// Dependencies: public customer contract and standard library; module composition
// calls New. Invariants: employee authority is never accepted and tokens are opaque.
package auth

import (
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
	"time"
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
}

var _ c.Service = (*Service)(nil)

// New is fail-closed until the tests-first CI baseline records failing behavior.
func New(cfg Config) (*Service, error) { return &Service{cfg: cfg}, nil }
