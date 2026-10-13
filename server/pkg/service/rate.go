package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
)

func requestPolicies(route string, identity model.Identity) []model.RatePolicy {
	tokenLimit, userLimit := 60, 120
	if route == "mark" {
		tokenLimit, userLimit = 10, 20
	}
	return []model.RatePolicy{
		{Policy: route + ".token.requests.minute", Subject: strconv.Itoa(identity.TokenID),
			Limit: tokenLimit, WindowSeconds: 60, Unit: "requests", Units: 1},
		{Policy: route + ".user.requests.minute", Subject: strconv.Itoa(identity.UserID),
			Limit: userLimit, WindowSeconds: 60, Unit: "requests", Units: 1},
	}
}

func accountPolicies(route string, identity model.Identity, count int) []model.RatePolicy {
	accountLimit := 6000
	if route == "mark" {
		accountLimit = 200
	}
	policies := []model.RatePolicy{
		{Policy: route + ".user.accounts.minute", Subject: strconv.Itoa(identity.UserID),
			Limit: accountLimit, WindowSeconds: 60, Unit: "accounts", Units: count},
	}
	if route == "mark" {
		policies = append(policies, model.RatePolicy{
			Policy: "mark.user.accounts.day", Subject: strconv.Itoa(identity.UserID),
			Limit: 1000, WindowSeconds: 86400, Unit: "accounts", Units: count,
		})
	}
	return policies
}

// admit checks every rolling policy before charging any of them. It is called
// inside a transaction, so other server processes cannot overspend.
func admit(ctx context.Context, store *data.Store, policies []model.RatePolicy) error {
	now := time.Now()
	var blocked *model.RateLimitError
	for _, policy := range policies {
		events, err := store.RateEvents(ctx, policy.Policy, policy.Subject, now)
		if err != nil {
			return errors.Join(model.ErrUnavailable, err)
		}
		used := policy.Units
		for _, event := range events {
			used += event.Units
		}
		if used <= policy.Limit {
			continue
		}
		retry := policy.WindowSeconds
		for _, event := range events {
			used -= event.Units
			if used <= policy.Limit {
				retry = max(1, int(math.Ceil(event.ExpiresAt.Sub(now).Seconds())))
				break
			}
		}
		if blocked == nil || retry > blocked.RetryAfterSeconds {
			blocked = &model.RateLimitError{RetryAfterSeconds: retry, RateLimit: policy}
		}
	}
	if blocked != nil {
		return blocked
	}
	for _, policy := range policies {
		err := store.CreateRateEvent(ctx, data.RateEvent{Policy: policy.Policy, Subject: policy.Subject,
			Units: policy.Units, ExpiresAt: now.Add(time.Duration(policy.WindowSeconds) * time.Second)})
		if err != nil {
			return errors.Join(model.ErrUnavailable, err)
		}
	}
	return nil
}

// AdmitRequests charges token and user request budgets before a protected route runs.
func (s *Service) AdmitRequests(ctx context.Context, route string, identity model.Identity) error {
	err := s.store.WithTx(ctx, func(tx *data.Store) error {
		return admit(ctx, tx, requestPolicies(route, identity))
	})
	var rate *model.RateLimitError
	if err != nil && !errors.As(err, &rate) {
		return errors.Join(model.ErrUnavailable, err)
	}
	return err
}

func (s *Service) AdmitIP(ctx context.Context, ip string, public bool) error {
	policy := model.RatePolicy{Policy: "auth.ip.requests.minute", Subject: ip,
		Limit: 20, WindowSeconds: 60, Unit: "requests", Units: 1}
	if public {
		policy.Policy, policy.Limit = "openapi.ip.requests.minute", 300
	}
	err := s.store.WithTx(ctx, func(tx *data.Store) error { return admit(ctx, tx, []model.RatePolicy{policy}) })
	var rate *model.RateLimitError
	if err != nil && !errors.As(err, &rate) {
		return errors.Join(model.ErrUnavailable, err)
	}
	return err
}
