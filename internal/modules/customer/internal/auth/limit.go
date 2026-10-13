// Responsibilities: bounded pre-database admission and opaque persistent rate keys.
// Dependencies: HMAC/SHA256, caller IP and repository atomic counters; auth
// commands call begin. Invariants: IPv6 /64 grouping, distinct tenant/domain keys,
// immediate429 without sleep, and no secret identifier is persisted in counters.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"net/netip"
	"time"
	c "github.com/luogangan7-lgtm/opensite-commerce/internal/modules/customer/contract"
)

func rateLimited(retry time.Duration) error {
	if retry<=0 {retry=time.Second};if retry>time.Minute {retry=time.Minute}
	return &c.RateLimitError{RetryAfter:retry}
}
func (s *Service) keyedHash(tenant,domain,value string) [32]byte {
	h:=hmac.New(sha256.New,s.cfg.Secret)
	h.Write([]byte("osc-customer-rate-v1\x00"+domain+"\x00"+tenant+"\x00"+value))
	var hash [32]byte;copy(hash[:],h.Sum(nil));return hash
}
func (s *Service) rateHashes(tenant,email string,ip netip.Addr) ([32]byte,[32]byte) {
	ip=ip.Unmap().WithZone("")
	if ip.Is6(){ip=netip.PrefixFrom(ip,64).Masked().Addr()}
	return s.keyedHash(tenant,"account",email),s.keyedHash(tenant,"ip",ip.String())
}
func (s *Service) begin(parent context.Context,tenant,account string,ip netip.Addr) (context.Context,func(),error) {
	if parent.Err()!=nil{return nil,nil,rateLimited(0)}
	select {case s.admission<-struct{}{}:default:return nil,nil,rateLimited(0)}
	ctx,cancel:=context.WithTimeout(parent,s.cfg.HashTimeout)
	finish:=func(){cancel();<-s.admission}
	accountHash,ipHash:=s.rateHashes(tenant,account,ip)
	retry,err:=s.cfg.Repo.Admit(ctx,tenant,accountHash,ipHash,s.cfg.Now())
	if ctx.Err()!=nil {finish();return nil,nil,rateLimited(0)}
	if err!=nil {finish();return nil,nil,c.ErrUnavailable}
	if retry>0 {finish();return nil,nil,rateLimited(retry)}
	return ctx,finish,nil
}
