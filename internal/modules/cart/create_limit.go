// Anonymous create admission belongs to cart's authentication boundary, called
// by bootstrap before persistence. Stdlib buckets depend only on direct peer IP
// and hashed capability; forwarding headers cannot mint quota or leak tokens.
package cart

import (
	"crypto/sha256"
	"net"
	"net/netip"
	"sync"
	"time"

	apperrors "github.com/luogangan7-lgtm/opensite-commerce/internal/platform/errors"
)

// AnonymousCreateLimit configures per-process quotas. Zero values use safe
// defaults; negative/oversized values also fall back rather than disable safety.
type AnonymousCreateLimit struct {
	IPBurst, TokenBurst int
	RefillInterval      time.Duration
	MaxKeys             int
}

type createBucket struct {
	tokens float64
	at     time.Time
	burst  int
}
type createLimiter struct {
	sync.Mutex
	config  AnonymousCreateLimit
	buckets map[[32]byte]createBucket
	now     func() time.Time
}

func newCreateLimiter(c AnonymousCreateLimit) *createLimiter {
	if c.IPBurst <= 0 || c.IPBurst > 1000000 {
		c.IPBurst = 30
	}
	if c.TokenBurst <= 0 || c.TokenBurst > 1000000 {
		c.TokenBurst = 10
	}
	if c.RefillInterval <= 0 {
		c.RefillInterval = time.Minute
	}
	if c.MaxKeys <= 0 || c.MaxKeys > 1000000 {
		c.MaxKeys = 10000
	}
	return &createLimiter{config: c, buckets: make(map[[32]byte]createBucket), now: time.Now}
}
func (l *createLimiter) refill(b createBucket, at time.Time) createBucket {
	if at.After(b.at) {
		b.tokens = min(float64(b.burst), b.tokens+float64(at.Sub(b.at))/float64(l.config.RefillInterval))
		b.at = at
	}
	return b
}
func (l *createLimiter) allow(remote string, tokenHash []byte) bool {
	peer, _, err := net.SplitHostPort(remote)
	if err != nil {
		peer = remote
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		peer = "unknown"
	} else {
		ip = ip.Unmap()
		if ip.Is6() {
			peer = netip.PrefixFrom(ip, 64).Masked().String()
		} else {
			peer = ip.String()
		}
	}
	keys := [][32]byte{sha256.Sum256([]byte("ip:" + peer))}
	bursts := []int{l.config.IPBurst}
	if len(tokenHash) > 0 {
		keys = append(keys, sha256.Sum256(append([]byte("token:"), tokenHash...)))
		bursts = append(bursts, l.config.TokenBurst)
	}
	l.Lock()
	defer l.Unlock()
	at := l.now()
	missing := 0
	for _, key := range keys {
		if _, ok := l.buckets[key]; !ok {
			missing++
		}
	}
	if len(l.buckets)+missing > l.config.MaxKeys {
		// ponytail: bounded map and one mutex; use a shared limiter when replicas
		// need a global quota. Only fully refilled buckets can be safely forgotten.
		for key, b := range l.buckets {
			if l.refill(b, at).tokens == float64(b.burst) {
				delete(l.buckets, key)
			}
		}
		missing = 0
		for _, key := range keys {
			if _, ok := l.buckets[key]; !ok {
				missing++
			}
		}
		if len(l.buckets)+missing > l.config.MaxKeys {
			return false
		}
	}
	next := make([]createBucket, len(keys))
	for i, key := range keys {
		b, ok := l.buckets[key]
		if !ok {
			b = createBucket{float64(bursts[i]), at, bursts[i]}
		}
		b = l.refill(b, at)
		if b.tokens < 1 {
			return false
		}
		next[i] = b
	}
	for i, key := range keys {
		b := next[i]
		b.tokens--
		l.buckets[key] = b
	}
	return true
}
func createLimited() error {
	return apperrors.New(429, "cart_create_rate_limited", "Anonymous cart creation rate exceeded")
}
