package controller

import (
	"net/http"
	"strconv"
	"time"

	"github.com/41tair/slopless/pkg/model"
)

const sessionCookie = "slopless_session"

type credentials struct {
	Username       string `json:"username"`
	Password       string `json:"password"`
	InvitationCode string `json:"invitationCode"`
}

func (h *Controller) credentials(c SContext, registering bool) (credentials, bool) {
	var input credentials
	fields := []string{"username", "password"}
	if registering {
		fields = append(fields, "invitationCode")
	}
	if !c.Bind(&input, fields...) {
		return input, false
	}
	return input, true
}

func (h *Controller) login(c SContext) {
	input, ok := h.credentials(c, false)
	if !ok {
		return
	}
	result, err := h.service.Login(c.req.Context(), input.Username, input.Password)
	if err != nil {
		c.failure(err)
		return
	}
	http.SetCookie(c.w, &http.Cookie{
		Name: sessionCookie, Value: result.Secret, Path: "/api/v1", Expires: result.ExpiresAt,
		HttpOnly: true, Secure: h.secureCookies || c.req.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	c.Resp(map[string]any{"data": result.User})
}

func (h *Controller) register(c SContext) {
	input, ok := h.credentials(c, true)
	if !ok {
		return
	}
	u, err := h.service.Register(c.req.Context(), model.Registration{
		Username: input.Username, Password: input.Password, InvitationCode: input.InvitationCode,
	})
	if err != nil {
		c.failure(err)
		return
	}
	c.JSON(http.StatusCreated, map[string]any{"data": u})
}

func (h *Controller) logout(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	if err := h.service.Logout(c.req.Context(), *identity); err != nil {
		c.failure(err)
		return
	}
	http.SetCookie(c.w, &http.Cookie{
		Name: sessionCookie, Path: "/api/v1", MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies || c.req.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	c.w.WriteHeader(http.StatusNoContent)
}

func (h *Controller) createAPIToken(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	var input struct {
		Name      string     `json:"name"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if !c.Bind(&input, "name", "expiresAt") {
		return
	}
	token, secret, err := h.service.CreateToken(c.req.Context(), model.TokenInput{
		UserID: identity.UserID, Name: input.Name, ExpiresAt: input.ExpiresAt,
	})
	if err != nil {
		c.failure(err)
		return
	}
	c.JSON(http.StatusCreated, map[string]any{"data": struct {
		*model.Token
		Secret string `json:"token"`
	}{Token: token, Secret: secret}})
}

func (h *Controller) deleteAPIToken(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	id, err := strconv.Atoi(c.req.PathValue("id"))
	if err != nil || id < 1 {
		c.invalid("/id", "invalid_id", "Provide a positive token ID.")
		return
	}
	if err := h.service.DeleteToken(c.req.Context(), identity.UserID, id); err != nil {
		c.failure(err)
		return
	}
	c.w.WriteHeader(http.StatusNoContent)
}

func (h *Controller) listAPIToken(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	tokens, err := h.service.Tokens(c.req.Context(), identity.UserID)
	if err != nil {
		c.failure(err)
		return
	}
	c.Resp(map[string]any{"data": tokens})
}

func (h *Controller) userInfo(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	u, err := h.service.User(c.req.Context(), identity.UserID)
	if err != nil {
		c.failure(err)
		return
	}
	c.Resp(map[string]any{"data": u})
}
