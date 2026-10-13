// Package service owns application rules and transaction boundaries. It has no
// dependency on HTTP or Ent; data.Store implements persistence operations.
package service

import (
	"context"
	"slices"

	"github.com/41tair/slopless/pkg/config"
	"github.com/41tair/slopless/pkg/data"
)

type Service struct {
	store   *data.Store
	credits config.Credits
}

func New(store *data.Store, credits config.Credits) (*Service, error) {
	if err := credits.Validate(); err != nil {
		return nil, err
	}
	credits.InvitationRewards = slices.Clone(credits.InvitationRewards)
	return &Service{store: store, credits: credits}, nil
}

func (s *Service) Cleanup(ctx context.Context) error { return s.store.Cleanup(ctx) }
