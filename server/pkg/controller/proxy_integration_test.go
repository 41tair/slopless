//go:build integration

package controller

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
)

func TestProxyRateLimitIsolation(t *testing.T) {
	t.Parallel()
	routes := []struct {
		name, path, body, policy string
		limit                    int
	}{
		{name: "login", path: "/api/v1/login", body: `{"username":"bob","password":"a-test-password"}`, policy: "auth.ip.requests.minute", limit: 20},
		{name: "openapi", path: "/openapi/v1/accounts/query", body: `{"accounts":[{"platform":"github","username":"example"}]}`, policy: "openapi.ip.requests.minute", limit: 300},
	}
	proxies := []struct {
		name     string
		prefixes []netip.Prefix
		isolated bool
	}{
		{name: "trusted", prefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, isolated: true},
		{name: "untrusted", prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}},
		{name: "disabled"},
	}
	for _, proxy := range proxies {
		for _, route := range routes {
			t.Run(proxy.name+"/"+route.name, func(t *testing.T) {
				t.Parallel()
				svc, original := fixture(t)
				handler := NewRouter(svc, true, proxy.prefixes...)
				invites, err := svc.BootstrapInvitations(t.Context(), 1)
				if err != nil {
					t.Fatal(err)
				}
				u, err := svc.Register(t.Context(), model.Registration{Username: "bob", Password: "a-test-password", InvitationCode: invites[0].Code})
				if err != nil {
					t.Fatal(err)
				}
				_, token, err := svc.CreateToken(t.Context(), model.TokenInput{UserID: u.ID, Name: "reader"})
				if err != nil {
					t.Fatal(err)
				}
				request := func(ip, body, secret string) *httptest.ResponseRecorder {
					req := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body))
					req.RemoteAddr = "127.0.0.1:50000"
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Forwarded-For", ip)
					if secret != "" {
						req.Header.Set("Authorization", "Bearer "+secret)
					}
					result := httptest.NewRecorder()
					handler.ServeHTTP(result, req)
					return result
				}
				// Seed the exhausted window in real D1. Hundreds of REST round trips
				// can outlast a one-minute window and test elapsed time instead of IP
				// isolation. Request charging itself is covered by the admission tests.
				subject := "127.0.0.1"
				if proxy.isolated {
					subject = "192.0.2.10"
				}
				if err := original.(*testHandler).store.CreateRateEvent(t.Context(), data.RateEvent{
					Policy: route.policy, Subject: subject, Units: route.limit,
					ExpiresAt: time.Now().Add(time.Minute),
				}); err != nil {
					t.Fatal(err)
				}
				requireStatus(t, request("192.0.2.10", "{}", ""), http.StatusTooManyRequests)
				// The exhausted client remains blocked even when prepending a forged IP.
				requireStatus(t, request("203.0.113.99, 192.0.2.10", "{}", ""), http.StatusTooManyRequests)
				want := http.StatusTooManyRequests
				if proxy.isolated {
					want = http.StatusOK
				}
				requireStatus(t, request("198.51.100.20", route.body, token), want)
			})
		}
	}
}
