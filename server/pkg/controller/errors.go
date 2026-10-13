package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/41tair/slopless/pkg/model"
)

func (c SContext) failure(err error) {
	status := http.StatusInternalServerError
	code, detail := "internal_error", "An unexpected error occurred."
	body := map[string]any{}
	var rate *model.RateLimitError
	var validation *model.ValidationError
	switch {
	case errors.As(err, &validation):
		status = http.StatusUnprocessableEntity
		code, detail = "validation_failed", "One or more request fields are invalid."
		body["errors"] = []fieldError{{Path: validation.Path, Code: validation.Code, Message: validation.Message}}
	case errors.As(err, &rate):
		status = http.StatusTooManyRequests
		code, detail = "rate_limited", "The request budget has been exceeded."
		c.w.Header().Set("Retry-After", strconv.Itoa(rate.RetryAfterSeconds))
		body["retryAfterSeconds"], body["rateLimit"] = rate.RetryAfterSeconds, rate.RateLimit
	case errors.Is(err, model.ErrUnauthorized):
		status = http.StatusUnauthorized
		code, detail = "unauthorized", "Invalid or expired credentials."
	case errors.Is(err, model.ErrUsernameTaken):
		status = http.StatusConflict
		code, detail = "username_taken", "The username is already registered."
	case errors.Is(err, model.ErrInvalidInvitation):
		status = http.StatusUnprocessableEntity
		code, detail = "invalid_invitation", "The invitation code is invalid or already used."
	case errors.Is(err, model.ErrInsufficientCredits):
		status = http.StatusConflict
		code, detail = "insufficient_credits", "Not enough credits to purchase an invitation."
	case errors.Is(err, model.ErrNotFound):
		status = http.StatusNotFound
		code, detail = "not_found", "The requested resource was not found."
	case errors.Is(err, model.ErrIdempotencyConflict):
		status = http.StatusConflict
		code, detail = "idempotency_conflict", "The key was used with different account input."
	case errors.Is(err, model.ErrUnavailable):
		status = http.StatusServiceUnavailable
		code, detail = "service_unavailable", "The database is temporarily unavailable."
		c.w.Header().Set("Retry-After", "1")
		body["retryAfterSeconds"] = 1
	}
	body["code"], body["detail"] = code, detail
	c.problem(status, body, err)
}
