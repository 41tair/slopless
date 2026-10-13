package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/41tair/slopless/pkg/model"
)

var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (h *Controller) query(c SContext) {
	identity, err := apiIdentity(c.req)
	if err != nil {
		c.failure(err)
		return
	}
	var input struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if !c.Bind(&input, "accounts") {
		return
	}
	accounts, ok := c.accounts(input.Accounts)
	if !ok {
		return
	}
	states, err := h.service.Query(c.req.Context(), *identity, accounts)
	if err != nil {
		c.failure(err)
		return
	}
	c.Resp(map[string]any{"data": states})
}

func (h *Controller) mark(c SContext) {
	identity, err := apiIdentity(c.req)
	if err != nil {
		c.failure(err)
		return
	}
	key := c.req.Header.Get("Idempotency-Key")
	if len(c.req.Header.Values("Idempotency-Key")) != 1 || !idempotencyPattern.MatchString(key) {
		c.Problem(
			http.StatusBadRequest, "invalid_idempotency_key",
			"Provide an Idempotency-Key of 16–128 letters, digits, hyphens or underscores.",
		)
		return
	}
	var input struct {
		Accounts []json.RawMessage `json:"accounts"`
		Marked   *bool             `json:"marked"`
	}
	if !c.Bind(&input, "accounts", "marked") {
		return
	}
	if input.Marked == nil {
		c.invalid("/marked", "required", "Provide a boolean marked field.")
		return
	}
	accounts, ok := c.accounts(input.Accounts)
	if !ok {
		return
	}
	states, replayed, err := h.service.Mark(c.req.Context(), *identity, model.MarkInput{
		Accounts: accounts, Marked: *input.Marked, Key: key,
	})
	if err != nil {
		c.failure(err)
		return
	}
	c.w.Header().Set("Idempotency-Replayed", strconv.FormatBool(replayed))
	c.Resp(map[string]any{"data": states})
}

func (c SContext) accounts(input []json.RawMessage) ([]model.Account, bool) {
	result := make([]model.Account, 0, len(input))
	for i, raw := range input {
		path := fmt.Sprintf("/accounts/%d", i)
		object := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, c.invalid(path, "invalid_account", "Provide an account object.")
		}
		for key := range object {
			if key != "platform" && key != "username" {
				return nil, c.invalid(path+"/"+pointerEscape(key), "unknown_field", "Unknown account field.")
			}
		}
		var account model.Account
		if err := json.Unmarshal(raw, &account); err != nil {
			return nil, c.invalid(path, "invalid_account", "Platform and username must be strings.")
		}
		result = append(result, account)
	}
	return result, true
}
