package data

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/rateevent"
)

type RateEvent struct {
	Policy    string
	Subject   string
	Units     int
	ExpiresAt time.Time
}

func (s *Store) RateEvents(ctx context.Context, policy, subject string, at time.Time) ([]RateEvent, error) {
	if s.plan != nil {
		return s.plan.rateEvents(ctx, policy, subject, at)
	}

	rows, err := s.client.RateEvent.Query().Where(
		rateevent.PolicyEQ(policy), rateevent.SubjectEQ(subject), rateevent.ExpiresAtGT(at),
	).
		Order(ent.Asc(rateevent.FieldExpiresAt)).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	result := make([]RateEvent, 0, len(rows))
	for _, r := range rows {
		result = append(result, RateEvent{Policy: r.Policy, Subject: r.Subject, Units: r.Units, ExpiresAt: r.ExpiresAt})
	}
	return result, nil
}

func (s *Store) CreateRateEvent(ctx context.Context, input RateEvent) error {
	if s.plan == nil {
		return s.WithTx(ctx, func(tx *Store) error {
			return tx.CreateRateEvent(ctx, input)
		})
	}
	if err := rateevent.UnitsValidator(input.Units); err != nil {
		return fmt.Errorf("validate rate event units: %w", err)
	}

	id, err := s.plan.nextID(ctx, rateevent.Table)
	if err != nil {
		return databaseError(err)
	}
	s.plan.insert(rateevent.Table, id, map[string]any{
		rateevent.FieldPolicy:    input.Policy,
		rateevent.FieldSubject:   input.Subject,
		rateevent.FieldUnits:     input.Units,
		rateevent.FieldExpiresAt: input.ExpiresAt,
	})
	s.plan.rateState().pending = append(s.plan.rateState().pending, input)
	return nil
}

func (p *mutationPlan) rateEvents(ctx context.Context, policy, subject string, at time.Time) ([]RateEvent, error) {
	state := p.rateState()
	key := rateQueryKey{policy: policy, subject: subject}
	cached, ok := state.queries[key]
	if !ok || at.Before(cached.at) {
		rows, err := p.base.RateEvents(ctx, policy, subject, at)
		if err != nil {
			return nil, err
		}
		cached = rateQuery{at: at, events: rows}
		state.queries[key] = cached
	}

	result := make([]RateEvent, 0, len(cached.events)+len(state.pending))
	for _, event := range cached.events {
		if event.ExpiresAt.After(at) {
			result = append(result, event)
		}
	}
	for _, event := range state.pending {
		if event.Policy == policy && event.Subject == subject && event.ExpiresAt.After(at) {
			result = append(result, event)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].ExpiresAt.Before(result[j].ExpiresAt)
	})
	return result, nil
}
