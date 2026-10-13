// Client address parsing owns the HTTP proxy trust boundary. Dependencies:
// stdlib net/netip and Handler configuration; login create calls it before DB.
// Only configured direct peers can supply XFF; Forwarded is unsupported.
package httpapi

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func (h *Handler) trusts(ip netip.Addr) bool {
	for _, prefix := range h.trusted {
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}
func (h *Handler) clientIP(r *http.Request) (netip.Addr, error) {
	invalid := errors.New("invalid client address")
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, invalid
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || peer.Zone() != "" {
		return netip.Addr{}, invalid
	}
	peer = peer.Unmap()
	values := r.Header.Values("X-Forwarded-For")
	if !h.trusts(peer) || len(values) == 0 {
		return peer, nil
	}
	// Bound allocation and parsing before joining repeated header fields. Multiple
	// fields have the same ordered chain semantics as a comma-separated field.
	size := len(values) - 1
	for _, value := range values {
		size += len(value)
		if size > 4096 {
			return netip.Addr{}, invalid
		}
	}
	hops := strings.Split(strings.Join(values, ","), ",")
	if len(hops) > 32 {
		return netip.Addr{}, invalid
	}
	addresses := make([]netip.Addr, len(hops))
	for i, hop := range hops {
		ip, err := netip.ParseAddr(strings.TrimSpace(hop))
		if err != nil || ip.Zone() != "" {
			return netip.Addr{}, invalid
		}
		addresses[i] = ip.Unmap()
	}
	client := peer
	for i := len(addresses) - 1; i >= 0 && h.trusts(client); i-- {
		client = addresses[i]
	}
	return client, nil
}
