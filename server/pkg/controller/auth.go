package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/41tair/slopless/pkg/middlewares"
	"github.com/41tair/slopless/pkg/model"
)

func (h *Controller) session(c SContext) (*model.Identity, bool) {
	cookie, err := c.req.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		c.failure(model.ErrUnauthorized)
		return nil, false
	}
	identity, err := h.service.AuthenticateSession(c.req.Context(), cookie.Value)
	if err != nil {
		c.failure(err)
		return nil, false
	}
	return identity, true
}

func (h *Controller) apiToken(c SContext) (*model.Identity, bool) {
	values := c.req.Header.Values("Authorization")
	parts := strings.Fields(c.req.Header.Get("Authorization"))
	validHeader := len(values) == 1 && len(parts) == 2
	if !validHeader || !strings.EqualFold(parts[0], "Bearer") {
		c.w.Header().Set("WWW-Authenticate", `Bearer realm="slopless"`)
		c.failure(model.ErrUnauthorized)
		return nil, false
	}
	identity, err := h.service.AuthenticateAPI(c.req.Context(), parts[1])
	if err != nil {
		if errors.Is(err, model.ErrUnauthorized) {
			c.w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		c.failure(err)
		return nil, false
	}
	return identity, true
}

func (h *Controller) ipRateLimit(public bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return middlewares.RateLimit(next, func(req *http.Request) error {
			return h.service.AdmitIP(req.Context(), h.clientIP(req), public)
		}, func(w http.ResponseWriter, req *http.Request, err error) {
			SContext{w: w, req: req}.failure(err)
		})
	}
}

type apiIdentityKey struct{}

func apiIdentity(req *http.Request) (*model.Identity, error) {
	identity, ok := req.Context().Value(apiIdentityKey{}).(*model.Identity)
	if !ok || identity == nil {
		return nil, model.ErrUnauthorized
	}
	return identity, nil
}

func (h *Controller) apiRequest(route string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		limited := middlewares.RateLimit(next, func(req *http.Request) error {
			identity, err := apiIdentity(req)
			if err != nil {
				return err
			}
			return h.service.AdmitRequests(req.Context(), route, *identity)
		}, func(w http.ResponseWriter, req *http.Request, err error) {
			SContext{w: w, req: req}.failure(err)
		})
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			identity, ok := h.apiToken(SContext{w: w, req: req})
			if !ok {
				return
			}
			ctx := context.WithValue(req.Context(), apiIdentityKey{}, identity)
			limited.ServeHTTP(w, req.WithContext(ctx))
		})
	}
}
