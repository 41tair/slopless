package controller

import (
	"net/http"
	"strconv"

	"github.com/41tair/slopless/pkg/model"
)

func (c SContext) page() (model.Page, bool) {
	page := model.Page{Limit: 20}
	query := c.req.URL.Query()
	if query.Has("limit") {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil {
			return page, c.invalid("/limit", "invalid_limit", "Limit must be between 1 and 100.")
		}
		page.Limit = limit
	}
	if query.Has("before") {
		before, err := strconv.Atoi(query.Get("before"))
		if err != nil || before < 1 {
			return page, c.invalid("/before", "invalid_cursor", "Provide a positive cursor ID.")
		}
		page.Before = before
	}
	return page, true
}

func (h *Controller) credits(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	page, ok := c.page()
	if !ok {
		return
	}
	result, err := h.service.Credits(c.req.Context(), identity.UserID, page)
	if err != nil {
		c.failure(err)
		return
	}
	c.Resp(map[string]any{"data": result})
}

func (h *Controller) purchaseInvitation(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	key := c.req.Header.Get("Idempotency-Key")
	if len(c.req.Header.Values("Idempotency-Key")) != 1 || !idempotencyPattern.MatchString(key) {
		c.Problem(http.StatusBadRequest, "invalid_idempotency_key", "Provide a valid Idempotency-Key for this purchase.")
		return
	}
	var input struct{}
	if !c.Bind(&input) {
		return
	}
	invite, replayed, err := h.service.PurchaseInvitation(c.req.Context(), identity.UserID, key)
	if err != nil {
		c.failure(err)
		return
	}
	c.w.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	c.JSON(http.StatusCreated, map[string]any{"data": invite})
}

func (h *Controller) listInvitations(c SContext) {
	identity, ok := h.session(c)
	if !ok {
		return
	}
	page, ok := c.page()
	if !ok {
		return
	}
	invites, next, err := h.service.Invitations(c.req.Context(), identity.UserID, page)
	if err != nil {
		c.failure(err)
		return
	}
	c.Resp(map[string]any{"data": invites, "nextCursor": next})
}
