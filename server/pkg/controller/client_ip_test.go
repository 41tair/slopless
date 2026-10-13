package controller

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("2001:db8:1::/64"),
	}
	tests := []struct {
		name      string
		peer      string
		forwarded []string
		want      string
	}{
		{name: "direct", peer: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "untrusted spoof", peer: "192.0.2.1:1234", forwarded: []string{"198.51.100.1"}, want: "192.0.2.1"},
		{name: "trusted proxy", peer: "127.0.0.1:1234", forwarded: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "missing header", peer: "127.0.0.1:1234", want: "127.0.0.1"},
		{name: "trusted chain", peer: "127.0.0.1:1234", forwarded: []string{" 198.51.100.1, 10.0.0.2 "}, want: "198.51.100.1"},
		{name: "multiple header lines", peer: "127.0.0.1:1234", forwarded: []string{"198.51.100.1", "10.0.0.2"}, want: "198.51.100.1"},
		{name: "untrusted intermediate", peer: "127.0.0.1:1234", forwarded: []string{"192.0.2.99, 198.51.100.1, 10.0.0.2"}, want: "198.51.100.1"},
		{name: "ignore attacker prefix", peer: "127.0.0.1:1234", forwarded: []string{"malformed, 198.51.100.1"}, want: "198.51.100.1"},
		{name: "empty nearest hop", peer: "127.0.0.1:1234", forwarded: []string{"198.51.100.1,"}, want: "127.0.0.1"},
		{name: "invalid nearest hop", peer: "127.0.0.1:1234", forwarded: []string{"198.51.100.1, invalid"}, want: "127.0.0.1"},
		{name: "all trusted", peer: "127.0.0.1:1234", forwarded: []string{"10.0.0.1, 10.0.0.2"}, want: "10.0.0.1"},
		{name: "direct IPv6", peer: "[2001:db8:2::1]:1234", want: "2001:db8:2::1"},
		{name: "trusted IPv6 proxy", peer: "[2001:db8:1::1]:1234", forwarded: []string{"2001:0db8:2::0001"}, want: "2001:db8:2::1"},
		{name: "mapped IPv4 peer", peer: "[::ffff:127.0.0.1]:1234", forwarded: []string{"198.51.100.1"}, want: "198.51.100.1"},
		{name: "mapped IPv4 client", peer: "127.0.0.1:1234", forwarded: []string{"::ffff:198.51.100.1"}, want: "198.51.100.1"},
		{name: "IPv6 peer zone", peer: "[fe80::1%en0]:1234", want: "fe80::1"},
		{name: "forwarded zone rejected", peer: "127.0.0.1:1234", forwarded: []string{"fe80::1%en0"}, want: "127.0.0.1"},
		{name: "forwarded port rejected", peer: "127.0.0.1:1234", forwarded: []string{"198.51.100.1:4321"}, want: "127.0.0.1"},
		{name: "bare peer", peer: "198.51.100.1", want: "198.51.100.1"},
		{name: "invalid peer", peer: "invalid", forwarded: []string{"198.51.100.1"}, want: "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &Controller{trustedProxies: trusted}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
			req.RemoteAddr = tt.peer
			for _, value := range tt.forwarded {
				req.Header.Add("X-Forwarded-For", value)
			}
			if got := h.clientIP(req); got != tt.want {
				t.Fatalf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPTrustsNoHeadersByDefault(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Real-IP", "198.51.100.2")
	req.Header.Set("Forwarded", "for=198.51.100.3")
	if got := (&Controller{}).clientIP(req); got != "127.0.0.1" {
		t.Fatalf("trusted spoofed headers: %q", got)
	}
}
