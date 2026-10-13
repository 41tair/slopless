package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/41tair/slopless/pkg/data"
	"github.com/41tair/slopless/pkg/model"
)

var githubPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}(\[bot\])?$`)

// New platforms supply their own validation and canonicalization here.
var platforms = map[string]func(string) (string, bool){
	"github": func(username string) (string, bool) {
		username = strings.TrimSpace(username)
		return strings.ToLower(username), githubPattern.MatchString(username)
	},
}

func normalizeAccounts(input []model.Account, maximum int) ([]model.Account, error) {
	if len(input) < 1 || len(input) > maximum {
		return nil, invalid("/accounts", "invalid_batch_size", fmt.Sprintf("Provide 1–%d accounts.", maximum))
	}
	result := make([]model.Account, 0, len(input))
	seen := make(map[model.Account]bool, len(input))
	for i, account := range input {
		path := fmt.Sprintf("/accounts/%d", i)
		normalize, ok := platforms[account.Platform]
		if !ok {
			return nil, invalid(path+"/platform", "unsupported_platform", "The platform is not enabled.")
		}
		if utf8.RuneCountInString(account.Username) > 128 {
			return nil, invalid(path+"/username", "invalid_username", "Username exceeds 128 characters.")
		}
		account.Username, ok = normalize(account.Username)
		if !ok {
			return nil, invalid(path+"/username", "invalid_username", "Provide a valid account username, not a profile URL.")
		}
		if !seen[account] {
			seen[account] = true
			result = append(result, account)
		}
	}
	return result, nil
}

func (s *Service) Query(
	ctx context.Context,
	identity model.Identity,
	accounts []model.Account,
) ([]model.AccountState, error) {
	accounts, err := normalizeAccounts(accounts, 100)
	if err != nil {
		return nil, err
	}
	result := []model.AccountState{}
	err = s.store.WithTx(ctx, func(tx *data.Store) error {
		if err := admit(ctx, tx, accountPolicies("query", identity, len(accounts))); err != nil {
			return err
		}
		var err error
		result, err = tx.Snapshot(ctx, identity.UserID, accounts)
		return err
	})
	return result, err
}

func (s *Service) Mark(
	ctx context.Context,
	identity model.Identity,
	input model.MarkInput,
) ([]model.AccountState, bool, error) {
	if input.Key == "" {
		return nil, false, fmt.Errorf("mark key is required")
	}
	accounts, err := normalizeAccounts(input.Accounts, 50)
	if err != nil {
		return nil, false, err
	}
	input.Accounts = accounts
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, false, err
	}
	hash := sha256.Sum256(payload)
	fingerprint := hex.EncodeToString(hash[:])
	result := []model.AccountState{}
	replayed := false
	err = s.store.WithTx(ctx, func(tx *data.Store) error {
		previous, err := tx.Replay(ctx, identity.UserID, input.Key)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			return err
		}
		if previous != nil && previous.ExpiresAt.After(time.Now()) {
			if previous.Fingerprint != fingerprint {
				return model.ErrIdempotencyConflict
			}
			if err := json.Unmarshal(previous.Response, &result); err != nil {
				return err
			}
			replayed = true
			return admit(ctx, tx, accountPolicies("mark", identity, len(accounts)))
		}
		if previous != nil {
			if err := tx.DeleteReplay(ctx, previous.ID); err != nil {
				return err
			}
		}
		if err := admit(ctx, tx, accountPolicies("mark", identity, len(accounts))); err != nil {
			return err
		}
		for _, account := range accounts {
			if err := s.setMark(ctx, tx, markChange{Account: account, UserID: identity.UserID, Marked: input.Marked}); err != nil {
				return err
			}
		}
		result, err = tx.Snapshot(ctx, identity.UserID, accounts)
		if err != nil {
			return err
		}
		response, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.SaveReplay(ctx, data.Replay{UserID: identity.UserID, Key: input.Key, Fingerprint: fingerprint,
			Response: response, ExpiresAt: time.Now().Add(48 * time.Hour)})
	})
	return result, replayed, err
}
