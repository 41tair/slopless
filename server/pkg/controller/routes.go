package controller

import (
	"net/http"
	"net/netip"
	"path"
	"slices"

	"github.com/41tair/slopless/pkg/middlewares"
	"github.com/41tair/slopless/pkg/service"
)

// Controller adapts HTTP requests to application services. Dependencies belong
// to the handler, while SContext contains only request/response state.
type Controller struct {
	service        *service.Service
	secureCookies  bool
	trustedProxies []netip.Prefix
}

func NewRouter(svc *service.Service, secureCookies bool, trustedProxies ...netip.Prefix) http.Handler {
	h := &Controller{service: svc, secureCookies: secureCookies, trustedProxies: slices.Clone(trustedProxies)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	web := NewGroup("api/v1", mux)
	web.NewHandler(
		"POST",
		"/login",
		h.login,
		h.ipRateLimit(false),
	)
	web.NewHandler(
		"POST",
		"/register",
		h.register,
		h.ipRateLimit(false),
	)
	web.NewHandler("POST", "/logout", h.logout)
	web.NewHandler("GET", "/user", h.userInfo)
	web.NewHandler("GET", "/credits", h.credits)
	web.NewHandler("POST", "/invitations", h.purchaseInvitation)
	web.NewHandler("GET", "/invitations", h.listInvitations)
	web.NewHandler("POST", "/tokens", h.createAPIToken)
	web.NewHandler("GET", "/tokens", h.listAPIToken)
	web.NewHandler("DELETE", "/tokens/{id}", h.deleteAPIToken)
	api := NewGroup("openapi/v1", mux)
	api.NewHandler(
		"POST",
		"/accounts/query",
		h.query,
		h.ipRateLimit(true),
		h.apiRequest("query"),
	)
	api.NewHandler(
		"POST",
		"/accounts/mark",
		h.mark,
		h.ipRateLimit(true),
		h.apiRequest("mark"),
	)
	return middlewares.RequestMetadata(mux)
}

type Group struct {
	name string
	mux  *http.ServeMux
}

func NewGroup(name string, mux *http.ServeMux) Group { return Group{name: name, mux: mux} }

func (g Group) NewHandler(method, route string, f func(SContext), middleware ...func(http.Handler) http.Handler) {
	handler := NewHandler(f)
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	if g.name == "api/v1" {
		protection := http.NewCrossOriginProtection()
		protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			SContext{w: w, req: req}.Problem(http.StatusForbidden, "cross_origin_request", "Cross-origin request rejected.")
		}))
		handler = protection.Handler(handler)
	}
	g.mux.Handle(method+" "+path.Join("/", g.name, route), handler)
}

func NewHandler(f func(SContext)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { f(SContext{w: w, req: req}) })
}
