package data

import (
	"context"
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
	_, err := s.client.RateEvent.Create().SetPolicy(input.Policy).SetSubject(input.Subject).
		SetUnits(input.Units).SetExpiresAt(input.ExpiresAt).Save(ctx)
	return databaseError(err)
}
