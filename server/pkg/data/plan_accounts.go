package data

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/account"
	"github.com/41tair/slopless/pkg/data/ent/confirmation"
	"github.com/41tair/slopless/pkg/data/ent/idempotency"
	"github.com/41tair/slopless/pkg/data/ent/mark"
	"github.com/41tair/slopless/pkg/model"
)

type replayKey struct {
	userID int
	key    string
}

type replayState struct {
	base    *Replay
	current *Replay
}

type markKey struct {
	userID  int
	account model.Account
}

type markState struct {
	id            int
	userID        int
	account       model.Account
	baseExists    bool
	currentExists bool
}

type attributionState struct {
	base    *Attribution
	current *Attribution
}

type confirmationKey struct {
	accountID int
	userID    int
}

type confirmationState struct {
	base    *Confirmation
	current *Confirmation
}

type plannedCreditEntry struct {
	id        int
	input     CreditRecord
	createdAt time.Time
}

type accountPlan struct {
	parent *mutationPlan

	replays    map[int]*replayState
	replayKeys map[replayKey]int
	marks      map[markKey]*markState

	attributions   map[model.Account]*attributionState
	attributionIDs map[int]*attributionState

	confirmations   map[int]*confirmationState
	confirmationIDs map[confirmationKey]int
	creditEntries   map[int]*plannedCreditEntry
}

func (p *mutationPlan) accountState() *accountPlan {
	if p.accounts == nil {
		p.accounts = &accountPlan{
			parent:          p,
			replays:         map[int]*replayState{},
			replayKeys:      map[replayKey]int{},
			marks:           map[markKey]*markState{},
			attributions:    map[model.Account]*attributionState{},
			attributionIDs:  map[int]*attributionState{},
			confirmations:   map[int]*confirmationState{},
			confirmationIDs: map[confirmationKey]int{},
			creditEntries:   map[int]*plannedCreditEntry{},
		}
	}
	return p.accounts
}

func cloneReplay(input *Replay) *Replay {
	if input == nil {
		return nil
	}
	result := *input
	result.Response = append([]byte(nil), input.Response...)
	return &result
}

func (p *accountPlan) replay(ctx context.Context, userID int, key string) (*Replay, error) {
	lookup := replayKey{userID: userID, key: key}
	if id, ok := p.replayKeys[lookup]; ok {
		if id == 0 || p.replays[id].current == nil {
			return nil, model.ErrNotFound
		}
		return cloneReplay(p.replays[id].current), nil
	}
	row, err := replayRecord(p.parent.base.client.Idempotency.Query().Where(
		idempotency.UserIDEQ(userID),
		idempotency.KeyEQ(key),
	).Only(ctx))
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			p.replayKeys[lookup] = 0
		}
		return nil, err
	}
	state := &replayState{base: cloneReplay(row), current: cloneReplay(row)}
	p.replays[row.ID] = state
	p.replayKeys[lookup] = row.ID
	return cloneReplay(row), nil
}

func (p *accountPlan) saveReplay(ctx context.Context, input Replay) error {
	id, err := p.parent.nextID(ctx, "idempotencies")
	if err != nil {
		return databaseError(err)
	}
	input.ID = id
	input.Response = append([]byte(nil), input.Response...)
	state := &replayState{current: cloneReplay(&input)}
	p.replays[id] = state
	p.replayKeys[replayKey{userID: input.UserID, key: input.Key}] = id
	p.parent.insert("idempotencies", id, map[string]any{
		"user_id":     input.UserID,
		"key":         input.Key,
		"fingerprint": input.Fingerprint,
		"response":    input.Response,
		"expires_at":  input.ExpiresAt,
	})
	return nil
}

func (p *accountPlan) deleteReplay(ctx context.Context, id int) error {
	state, ok := p.replays[id]
	if !ok {
		row, err := replayRecord(p.parent.base.client.Idempotency.Get(ctx, id))
		if err != nil {
			return err
		}
		state = &replayState{base: cloneReplay(row), current: cloneReplay(row)}
		p.replays[id] = state
		p.replayKeys[replayKey{userID: row.UserID, key: row.Key}] = id
	}
	if state.current == nil {
		return model.ErrNotFound
	}
	state.current = nil
	p.parent.delete("idempotencies", id)
	return nil
}

func (p *accountPlan) loadMark(ctx context.Context, userID int, target model.Account) (*markState, error) {
	key := markKey{userID: userID, account: target}
	if state, ok := p.marks[key]; ok {
		return state, nil
	}
	row, err := p.parent.base.client.Mark.Query().Where(
		mark.UserIDEQ(userID),
		mark.PlatformEQ(target.Platform),
		mark.UsernameEQ(target.Username),
	).Only(ctx)
	if err != nil && !errors.Is(databaseError(err), model.ErrNotFound) {
		return nil, databaseError(err)
	}
	state := &markState{userID: userID, account: target}
	if err == nil {
		state.id = row.ID
		state.baseExists = true
		state.currentExists = true
	}
	p.marks[key] = state
	return state, nil
}

func (p *accountPlan) hasMark(ctx context.Context, userID int, target model.Account) (bool, error) {
	state, err := p.loadMark(ctx, userID, target)
	if err != nil {
		return false, err
	}
	return state.currentExists, nil
}

func (p *accountPlan) createMark(ctx context.Context, userID int, target model.Account) error {
	if err := mark.PlatformValidator(target.Platform); err != nil {
		return err
	}
	if err := mark.UsernameValidator(target.Username); err != nil {
		return err
	}
	state, err := p.loadMark(ctx, userID, target)
	if err != nil {
		return err
	}
	if state.currentExists {
		return ErrConflict
	}
	if state.baseExists {
		state.currentExists = true
		p.parent.cancel("marks", state.id)
		return nil
	}
	if state.id == 0 {
		state.id, err = p.parent.nextID(ctx, "marks")
		if err != nil {
			return databaseError(err)
		}
	}
	state.currentExists = true
	p.parent.insert("marks", state.id, map[string]any{
		"user_id":  userID,
		"platform": target.Platform,
		"username": target.Username,
	})
	return nil
}

func (p *accountPlan) deleteMark(ctx context.Context, userID int, target model.Account) error {
	state, err := p.loadMark(ctx, userID, target)
	if err != nil {
		return err
	}
	if !state.currentExists {
		return nil
	}
	state.currentExists = false
	p.parent.delete("marks", state.id)
	return nil
}

func cloneAttribution(input *Attribution) *Attribution {
	if input == nil {
		return nil
	}
	result := *input
	result.FirstMarkerID = cloneInt(input.FirstMarkerID)
	return &result
}

func (p *accountPlan) rememberAttribution(value *Attribution) *attributionState {
	state := &attributionState{base: cloneAttribution(value), current: cloneAttribution(value)}
	p.attributions[value.Account] = state
	p.attributionIDs[value.ID] = state
	return state
}

func (p *accountPlan) attribution(ctx context.Context, target model.Account) (*Attribution, error) {
	if state, ok := p.attributions[target]; ok {
		if state.current == nil {
			return nil, model.ErrNotFound
		}
		return cloneAttribution(state.current), nil
	}
	value, err := attributionRecord(p.parent.base.client.Account.Query().Where(
		account.PlatformEQ(target.Platform),
		account.UsernameEQ(target.Username),
	).Only(ctx))
	if err != nil {
		return nil, err
	}
	p.rememberAttribution(value)
	return cloneAttribution(value), nil
}

func (p *accountPlan) attributionByID(ctx context.Context, id int) (*attributionState, error) {
	if state, ok := p.attributionIDs[id]; ok {
		if state.current == nil {
			return nil, model.ErrNotFound
		}
		return state, nil
	}
	value, err := attributionRecord(p.parent.base.client.Account.Get(ctx, id))
	if err != nil {
		return nil, err
	}
	return p.rememberAttribution(value), nil
}

func (p *accountPlan) createAttribution(
	ctx context.Context,
	target model.Account,
	userID int,
) (*Attribution, error) {
	if err := account.PlatformValidator(target.Platform); err != nil {
		return nil, err
	}
	if err := account.UsernameValidator(target.Username); err != nil {
		return nil, err
	}
	if existing, ok := p.attributions[target]; ok && existing.current != nil {
		return nil, ErrConflict
	}
	id, err := p.parent.nextID(ctx, "accounts")
	if err != nil {
		return nil, databaseError(err)
	}
	firstMarkerID := userID
	value := &Attribution{ID: id, Account: target, FirstMarkerID: &firstMarkerID}
	state := &attributionState{current: cloneAttribution(value)}
	p.attributions[target] = state
	p.attributionIDs[id] = state
	p.parent.insert("accounts", id, map[string]any{
		"platform":               target.Platform,
		"username":               target.Username,
		"rewarded_confirmations": 0,
		"awarded_credits":        int64(0),
		"created_at":             p.parent.now,
		"first_marker_id":        userID,
	})
	return cloneAttribution(value), nil
}

func (p *accountPlan) addAccountReward(ctx context.Context, id int, amount int64) (*Attribution, error) {
	state, err := p.attributionByID(ctx, id)
	if err != nil {
		return nil, err
	}
	value := cloneAttribution(state.current)
	value.RewardedConfirmations++
	value.AwardedCredits += amount
	state.current = value
	p.parent.update("accounts", id, map[string]any{
		"rewarded_confirmations": value.RewardedConfirmations,
		"awarded_credits":        value.AwardedCredits,
	})
	return cloneAttribution(value), nil
}

func cloneConfirmation(input *Confirmation) *Confirmation {
	if input == nil {
		return nil
	}
	result := *input
	result.RewardedAt = cloneTime(input.RewardedAt)
	return &result
}

func confirmationValue(row *ent.Confirmation) Confirmation {
	return Confirmation{
		ID:         row.ID,
		AccountID:  row.AccountID,
		UserID:     row.UserID,
		BranchID:   row.BranchID,
		RewardedAt: cloneTime(row.RewardedAt),
	}
}

func (p *accountPlan) rememberConfirmation(value *Confirmation) *confirmationState {
	if existing, ok := p.confirmations[value.ID]; ok {
		return existing
	}
	state := &confirmationState{base: cloneConfirmation(value), current: cloneConfirmation(value)}
	p.confirmations[value.ID] = state
	p.confirmationIDs[confirmationKey{accountID: value.AccountID, userID: value.UserID}] = value.ID
	return state
}

func (p *accountPlan) hasConfirmation(ctx context.Context, accountID, userID int) (bool, error) {
	key := confirmationKey{accountID: accountID, userID: userID}
	if id, ok := p.confirmationIDs[key]; ok {
		return id != 0 && p.confirmations[id].current != nil, nil
	}
	row, err := p.parent.base.client.Confirmation.Query().Where(
		confirmation.AccountIDEQ(accountID),
		confirmation.UserIDEQ(userID),
	).Only(ctx)
	if err != nil {
		if errors.Is(databaseError(err), model.ErrNotFound) {
			p.confirmationIDs[key] = 0
			return false, nil
		}
		return false, databaseError(err)
	}
	value := confirmationValue(row)
	p.rememberConfirmation(&value)
	return true, nil
}

func (p *accountPlan) createConfirmation(ctx context.Context, input Confirmation) error {
	key := confirmationKey{accountID: input.AccountID, userID: input.UserID}
	if id, ok := p.confirmationIDs[key]; ok && id != 0 && p.confirmations[id].current != nil {
		return ErrConflict
	}
	id, err := p.parent.nextID(ctx, "confirmations")
	if err != nil {
		return databaseError(err)
	}
	input.ID = id
	input.RewardedAt = nil
	state := &confirmationState{current: cloneConfirmation(&input)}
	p.confirmations[id] = state
	p.confirmationIDs[key] = id
	p.parent.insert("confirmations", id, map[string]any{
		"account_id":  input.AccountID,
		"user_id":     input.UserID,
		"branch_id":   input.BranchID,
		"created_at":  p.parent.now,
		"rewarded_at": nullableTime(input.RewardedAt),
	})
	return nil
}

func (p *accountPlan) confirmationsFor(ctx context.Context, accountID int) ([]Confirmation, error) {
	rows, err := p.parent.base.client.Confirmation.Query().Where(
		confirmation.AccountIDEQ(accountID),
	).All(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	for _, row := range rows {
		value := confirmationValue(row)
		p.rememberConfirmation(&value)
	}
	result := make([]Confirmation, 0, len(rows))
	for _, state := range p.confirmations {
		if state.current != nil && state.current.AccountID == accountID {
			result = append(result, *cloneConfirmation(state.current))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (p *accountPlan) confirmationByID(ctx context.Context, id int) (*confirmationState, error) {
	state, ok := p.confirmations[id]
	if ok {
		if state.current == nil {
			return nil, model.ErrNotFound
		}
		return state, nil
	}
	row, err := p.parent.base.client.Confirmation.Get(ctx, id)
	if err != nil {
		return nil, databaseError(err)
	}
	value := confirmationValue(row)
	return p.rememberConfirmation(&value), nil
}

func (p *accountPlan) rewardConfirmation(ctx context.Context, id int, at time.Time) error {
	state, err := p.confirmationByID(ctx, id)
	if err != nil {
		return err
	}
	value := cloneConfirmation(state.current)
	value.RewardedAt = &at
	state.current = value
	p.parent.update("confirmations", id, map[string]any{"rewarded_at": at})
	return nil
}

func (p *accountPlan) creditEntry(ctx context.Context, pending *plannedCreditEntry) (model.CreditEntry, error) {
	result := model.CreditEntry{
		ID:             pending.id,
		Amount:         pending.input.Amount,
		BalanceAfter:   pending.input.BalanceAfter,
		Reason:         pending.input.Reason,
		ConfirmationID: cloneInt(pending.input.ConfirmationID),
		InvitationID:   cloneInt(pending.input.InvitationID),
		CreatedAt:      pending.createdAt,
	}
	if pending.input.AccountID != nil {
		state, err := p.attributionByID(ctx, *pending.input.AccountID)
		if err != nil {
			return model.CreditEntry{}, err
		}
		account := state.current.Account
		result.Account = &account
	}
	if pending.input.ConfirmationID != nil {
		state, err := p.confirmationByID(ctx, *pending.input.ConfirmationID)
		if err != nil {
			return model.CreditEntry{}, err
		}
		confirmedByID := state.current.UserID
		result.ConfirmedByID = &confirmedByID
	}
	return result, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
