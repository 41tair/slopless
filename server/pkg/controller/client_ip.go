package controller

import (
	"net/http"
	"net/netip"
	"strings"
)

func (h *Controller) trustsProxy(addr netip.Addr) bool {
	for _, prefix := range h.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (h *Controller) clientIP(req *http.Request) string {
	host := req.RemoteAddr
	if addr, err := netip.ParseAddrPort(host); err == nil {
		host = addr.Addr().String()
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return req.RemoteAddr
	}
	peer = peer.WithZone("").Unmap()
	if !h.trustsProxy(peer) {
		return peer.String()
	}

	// Walk from the nearest proxy towards the client. Everything to the left of
	// the first untrusted hop can have been supplied by that hop and is ignored.
	forwarded := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
	client := peer
	for i := len(forwarded) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(forwarded[i]))
		if err != nil || addr.Zone() != "" {
			return peer.String()
		}
		client = addr.Unmap()
		if !h.trustsProxy(client) {
			return client.String()
		}
	}
	return client.String()
}
