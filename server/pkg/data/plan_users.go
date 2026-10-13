package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/41tair/slopless/pkg/d1"
	"github.com/41tair/slopless/pkg/data/ent"
	"github.com/41tair/slopless/pkg/data/ent/credential"
	"github.com/41tair/slopless/pkg/data/ent/invitation"
	"github.com/41tair/slopless/pkg/data/ent/user"
	"github.com/41tair/slopless/pkg/model"
)

type credentialLookup struct {
	hash string
	kind string
}

type invitationPurchaseLookup struct {
	issuerID int
	key      string
}

type plannedInvitation struct {
	invitation  model.Invitation
	purchaseKey string
}

type userPlan struct {
	usersByID              map[int]*UserRecord
	usersByName            map[string]*UserRecord
	usersByInvitation      map[int]*UserRecord
	missingUserNames       map[string]struct{}
	credentialsByID        map[int]*Credential
	credentialsByHash      map[string]*Credential
	missingCredentials     map[credentialLookup]struct{}
	deletedCredentials     map[int]struct{}
	invitationsByID        map[int]*plannedInvitation
	invitationsByCode      map[string]*plannedInvitation
	invitationsByPurchase  map[invitationPurchaseLookup]*plannedInvitation
	missingInvitationCodes map[string]struct{}
	missingInvitationBuys  map[invitationPurchaseLookup]struct{}
}

func (p *mutationPlan) userState() *userPlan {
	if p.users == nil {
		p.users = &userPlan{
			usersByID:              map[int]*UserRecord{},
			usersByName:            map[string]*UserRecord{},
			usersByInvitation:      map[int]*UserRecord{},
			missingUserNames:       map[string]struct{}{},
			credentialsByID:        map[int]*Credential{},
			credentialsByHash:      map[string]*Credential{},
			missingCredentials:     map[credentialLookup]struct{}{},
			deletedCredentials:     map[int]struct{}{},
			invitationsByID:        map[int]*plannedInvitation{},
			invitationsByCode:      map[string]*plannedInvitation{},
			invitationsByPurchase:  map[invitationPurchaseLookup]*plannedInvitation{},
			missingInvitationCodes: map[string]struct{}{},
			missingInvitationBuys:  map[invitationPurchaseLookup]struct{}{},
		}
	}
	return p.users
}

func (p *mutationPlan) timestamp() time.Time {
	if !p.now.IsZero() {
		return p.now
	}
	return time.Now()
}

func (p *mutationPlan) user(ctx context.Context, id int) (*UserRecord, error) {
	state := p.userState()
	if record, ok := state.usersByID[id]; ok {
		return cloneUserRecord(record), nil
	}
	record, err := p.base.User(ctx, id)
	if err != nil {
		return nil, err
	}
	state.cacheUser(record)
	return cloneUserRecord(record), nil
}

func (p *mutationPlan) userByName(ctx context.Context, name string) (*UserRecord, error) {
	state := p.userState()
	if record, ok := state.usersByName[name]; ok {
		return cloneUserRecord(record), nil
	}
	if _, ok := state.missingUserNames[name]; ok {
		return nil, model.ErrNotFound
	}
	record, err := p.base.UserByName(ctx, name)
	if errors.Is(err, model.ErrNotFound) {
		state.missingUserNames[name] = struct{}{}
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	state.cacheUser(record)
	return cloneUserRecord(record), nil
}

func (p *mutationPlan) createUser(ctx context.Context, input UserRecord) (*model.User, error) {
	if err := user.UsernameValidator(input.Username); err != nil {
		return nil, fmt.Errorf("validate username: %w", err)
	}
	if _, err := p.userByName(ctx, input.Username); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}

	if input.InvitationID != nil {
		used, err := p.userByInvitation(ctx, *input.InvitationID)
		if err != nil {
			return nil, err
		}
		if used != nil {
			return nil, ErrConflict
		}
	}

	id, err := p.nextID(ctx, user.Table)
	if err != nil {
		return nil, err
	}
	record := &UserRecord{
		User: model.User{
			ID:           id,
			Username:     input.Username,
			CreatedAt:    p.timestamp(),
			Credits:      0,
			InviterID:    cloneInt(input.InviterID),
			InvitationID: cloneInt(input.InvitationID),
		},
		PasswordHash: input.PasswordHash,
	}
	p.insert(user.Table, id, map[string]any{
		user.FieldUsername:     record.Username,
		user.FieldPasswordHash: record.PasswordHash,
		user.FieldCredits:      record.Credits,
		user.FieldCreatedAt:    record.CreatedAt,
		user.FieldInvitationID: nullableInt(record.InvitationID),
		user.FieldInviterID:    nullableInt(record.InviterID),
	})
	state := p.userState()
	state.cacheUser(record)
	delete(state.missingUserNames, record.Username)

	if record.InvitationID != nil {
		invite, loadErr := p.invitationByID(ctx, *record.InvitationID)
		if loadErr == nil {
			invite.invitation.RedeemerID = cloneInt(&id)
			state.cacheInvitation(invite)
		} else if !errors.Is(loadErr, model.ErrNotFound) {
			return nil, loadErr
		}
	}
	result := record.User
	return cloneUser(&result), nil
}

func (p *mutationPlan) userByInvitation(ctx context.Context, invitationID int) (*UserRecord, error) {
	state := p.userState()
	if record, ok := state.usersByInvitation[invitationID]; ok {
		return cloneUserRecord(record), nil
	}
	record, err := userRecord(p.base.client.User.Query().Where(user.InvitationIDEQ(invitationID)).Only(ctx))
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state.cacheUser(record)
	return cloneUserRecord(record), nil
}

func (p *mutationPlan) setBalance(ctx context.Context, userID int, balance int64) error {
	if err := user.CreditsValidator(balance); err != nil {
		return fmt.Errorf("validate user credits: %w", err)
	}
	record, err := p.user(ctx, userID)
	if err != nil {
		return err
	}
	record.Credits = balance
	p.userState().cacheUser(record)
	p.update(user.Table, userID, map[string]any{user.FieldCredits: balance})
	return nil
}

func (p *mutationPlan) createCredential(ctx context.Context, input Credential) (*Credential, error) {
	if err := credential.KindValidator(credential.Kind(input.Kind)); err != nil {
		return nil, fmt.Errorf("validate credential kind: %w", err)
	}
	state := p.userState()
	if existing, ok := state.credentialsByHash[input.SecretHash]; ok {
		if _, deleted := state.deletedCredentials[existing.ID]; !deleted {
			return nil, ErrConflict
		}
	}
	row, err := p.base.client.Credential.Query().Where(credential.SecretHashEQ(input.SecretHash)).Only(ctx)
	if err == nil {
		existing, convertErr := credentialRecord(row, nil)
		if convertErr != nil {
			return nil, convertErr
		}
		state.cacheCredential(existing)
		if _, deleted := state.deletedCredentials[existing.ID]; !deleted {
			return nil, ErrConflict
		}
	} else if !ent.IsNotFound(err) {
		return nil, databaseError(err)
	}

	id, err := p.nextID(ctx, credential.Table)
	if err != nil {
		return nil, err
	}
	record := &Credential{
		ID:         id,
		UserID:     input.UserID,
		Kind:       input.Kind,
		Name:       input.Name,
		SecretHash: input.SecretHash,
		CreatedAt:  p.timestamp(),
		ExpiresAt:  input.ExpiresAt,
	}
	p.insert(credential.Table, id, map[string]any{
		credential.FieldKind:       record.Kind,
		credential.FieldName:       record.Name,
		credential.FieldSecretHash: record.SecretHash,
		credential.FieldCreatedAt:  record.CreatedAt,
		credential.FieldExpiresAt:  record.ExpiresAt,
		credential.FieldUserID:     record.UserID,
	})
	state.cacheCredential(record)
	delete(state.missingCredentials, credentialLookup{hash: record.SecretHash, kind: record.Kind})
	return cloneCredential(record), nil
}

func (p *mutationPlan) credential(ctx context.Context, hash, kind string) (*Credential, error) {
	state := p.userState()
	if record, ok := state.credentialsByHash[hash]; ok {
		if _, deleted := state.deletedCredentials[record.ID]; deleted || record.Kind != kind {
			return nil, model.ErrNotFound
		}
		return cloneCredential(record), nil
	}
	lookup := credentialLookup{hash: hash, kind: kind}
	if _, ok := state.missingCredentials[lookup]; ok {
		return nil, model.ErrNotFound
	}
	record, err := p.base.Credential(ctx, hash, kind)
	if errors.Is(err, model.ErrNotFound) {
		state.missingCredentials[lookup] = struct{}{}
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	state.cacheCredential(record)
	if _, deleted := state.deletedCredentials[record.ID]; deleted {
		return nil, model.ErrNotFound
	}
	return cloneCredential(record), nil
}

func (p *mutationPlan) credentialByID(ctx context.Context, id int) (*Credential, error) {
	state := p.userState()
	if _, deleted := state.deletedCredentials[id]; deleted {
		return nil, model.ErrNotFound
	}
	if record, ok := state.credentialsByID[id]; ok {
		return cloneCredential(record), nil
	}
	row, err := p.base.client.Credential.Get(ctx, id)
	record, err := credentialRecord(row, err)
	if err != nil {
		return nil, err
	}
	state.cacheCredential(record)
	return cloneCredential(record), nil
}

func (p *mutationPlan) tokens(ctx context.Context, userID int) ([]*model.Token, error) {
	base, err := p.base.Tokens(ctx, userID)
	if err != nil {
		return nil, err
	}
	state := p.userState()
	byID := make(map[int]*model.Token, len(base)+len(state.credentialsByID))
	for _, token := range base {
		if _, deleted := state.deletedCredentials[token.ID]; deleted {
			continue
		}
		byID[token.ID] = cloneToken(token)
	}
	for id, record := range state.credentialsByID {
		if _, deleted := state.deletedCredentials[id]; deleted {
			delete(byID, id)
			continue
		}
		if record.UserID != userID || record.Kind != string(credential.KindAPI) {
			continue
		}
		byID[id] = &model.Token{ID: id, Name: record.Name, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt}
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]*model.Token, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneToken(byID[id]))
	}
	return result, nil
}

func (p *mutationPlan) deleteToken(ctx context.Context, userID, tokenID int) error {
	record, err := p.credentialByID(ctx, tokenID)
	if errors.Is(err, model.ErrNotFound) {
		return model.ErrNotFound
	}
	if err != nil {
		return err
	}
	if record.UserID != userID || record.Kind != string(credential.KindAPI) {
		return model.ErrNotFound
	}
	p.deleteCredential(record)
	return nil
}

func (p *mutationPlan) deleteSession(ctx context.Context, id int) error {
	record, err := p.credentialByID(ctx, id)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Kind != string(credential.KindSession) {
		return nil
	}
	p.deleteCredential(record)
	return nil
}

func (p *mutationPlan) deleteCredential(record *Credential) {
	state := p.userState()
	state.deletedCredentials[record.ID] = struct{}{}
	delete(state.missingCredentials, credentialLookup{hash: record.SecretHash, kind: record.Kind})
	p.delete(credential.Table, record.ID)
}

func (p *mutationPlan) invitationByID(ctx context.Context, id int) (*plannedInvitation, error) {
	state := p.userState()
	if record, ok := state.invitationsByID[id]; ok {
		return clonePlannedInvitation(record), nil
	}
	row, err := p.base.client.Invitation.Query().Where(invitation.IDEQ(id)).WithRedeemer().Only(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	record := plannedInvitationFromEnt(row)
	state.cacheInvitation(record)
	return clonePlannedInvitation(record), nil
}

func (p *mutationPlan) invitationByCode(ctx context.Context, code string) (*model.Invitation, error) {
	state := p.userState()
	if record, ok := state.invitationsByCode[code]; ok {
		return cloneInvitation(&record.invitation), nil
	}
	if _, ok := state.missingInvitationCodes[code]; ok {
		return nil, model.ErrNotFound
	}
	row, err := p.base.client.Invitation.Query().Where(invitation.CodeEQ(code)).WithRedeemer().Only(ctx)
	if ent.IsNotFound(err) {
		state.missingInvitationCodes[code] = struct{}{}
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, databaseError(err)
	}
	record := plannedInvitationFromEnt(row)
	state.cacheInvitation(record)
	return cloneInvitation(&record.invitation), nil
}

func (p *mutationPlan) invitationByPurchase(ctx context.Context, userID int, key string) (*model.Invitation, error) {
	state := p.userState()
	lookup := invitationPurchaseLookup{issuerID: userID, key: key}
	if record, ok := state.invitationsByPurchase[lookup]; ok {
		return cloneInvitation(&record.invitation), nil
	}
	if _, ok := state.missingInvitationBuys[lookup]; ok {
		return nil, model.ErrNotFound
	}
	row, err := p.base.client.Invitation.Query().Where(
		invitation.IssuerIDEQ(userID),
		invitation.PurchaseKeyEQ(key),
	).WithRedeemer().Only(ctx)
	if ent.IsNotFound(err) {
		state.missingInvitationBuys[lookup] = struct{}{}
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, databaseError(err)
	}
	record := plannedInvitationFromEnt(row)
	state.cacheInvitation(record)
	return cloneInvitation(&record.invitation), nil
}

func (p *mutationPlan) createInvitation(
	ctx context.Context,
	input model.Invitation,
	key *string,
) (*model.Invitation, error) {
	if err := invitation.CodeValidator(input.Code); err != nil {
		return nil, fmt.Errorf("validate invitation code: %w", err)
	}
	if _, err := p.invitationByCode(ctx, input.Code); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}

	purchaseKey := ""
	if key != nil {
		purchaseKey = *key
	}
	if input.IssuerID != nil {
		if _, err := p.invitationByPurchase(ctx, *input.IssuerID, purchaseKey); err == nil {
			return nil, ErrConflict
		} else if !errors.Is(err, model.ErrNotFound) {
			return nil, err
		}
	}
	if err := invitation.CostValidator(input.Cost); err != nil {
		return nil, fmt.Errorf("validate invitation cost: %w", err)
	}

	id, err := p.nextID(ctx, invitation.Table)
	if err != nil {
		return nil, err
	}
	record := &plannedInvitation{
		invitation: model.Invitation{
			ID:        id,
			Code:      input.Code,
			IssuerID:  cloneInt(input.IssuerID),
			Cost:      input.Cost,
			CreatedAt: p.timestamp(),
		},
		purchaseKey: purchaseKey,
	}
	p.insert(invitation.Table, id, map[string]any{
		invitation.FieldCode:        record.invitation.Code,
		invitation.FieldPurchaseKey: record.purchaseKey,
		invitation.FieldCost:        record.invitation.Cost,
		invitation.FieldCreatedAt:   record.invitation.CreatedAt,
		invitation.FieldUsedAt:      nil,
		invitation.FieldIssuerID:    nullableInt(record.invitation.IssuerID),
	})
	state := p.userState()
	state.cacheInvitation(record)
	delete(state.missingInvitationCodes, record.invitation.Code)
	if record.invitation.IssuerID != nil {
		delete(state.missingInvitationBuys, invitationPurchaseLookup{
			issuerID: *record.invitation.IssuerID,
			key:      record.purchaseKey,
		})
	}
	return cloneInvitation(&record.invitation), nil
}

func (p *mutationPlan) createInvitations(
	ctx context.Context,
	inputs []model.Invitation,
) ([]*model.Invitation, error) {
	result := make([]*model.Invitation, 0, len(inputs))
	if len(inputs) == 0 {
		return result, nil
	}

	state := p.userState()
	codes := make([]string, 0, len(inputs))
	requested := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if err := invitation.CodeValidator(input.Code); err != nil {
			return nil, fmt.Errorf("validate invitation code: %w", err)
		}
		if err := invitation.CostValidator(input.Cost); err != nil {
			return nil, fmt.Errorf("validate invitation cost: %w", err)
		}
		if _, exists := requested[input.Code]; exists {
			return nil, ErrConflict
		}
		if _, exists := state.invitationsByCode[input.Code]; exists {
			return nil, ErrConflict
		}
		requested[input.Code] = struct{}{}
		codes = append(codes, input.Code)
	}

	encoded, err := json.Marshal(codes)
	if err != nil {
		return nil, fmt.Errorf("encode invitation codes: %w", err)
	}
	rows, err := p.base.driver.Read(ctx, d1.Statement{
		SQL:    `SELECT "code" FROM "invitations" WHERE "code" IN (SELECT value FROM json_each(?))`,
		Params: []any{string(encoded)},
	})
	if err != nil {
		return nil, databaseError(err)
	}
	codeColumn := -1
	for index, column := range rows.Columns {
		if column == invitation.FieldCode {
			codeColumn = index
			break
		}
	}
	if codeColumn < 0 {
		return nil, databaseError(fmt.Errorf("%w: invitation code query omitted its column", d1.ErrUnavailable))
	}
	for _, row := range rows.Rows {
		if codeColumn >= len(row) {
			return nil, databaseError(fmt.Errorf("%w: invitation code query returned a short row", d1.ErrUnavailable))
		}
		code, ok := row[codeColumn].(string)
		if !ok {
			return nil, databaseError(fmt.Errorf("%w: invitation code query returned %T", d1.ErrUnavailable, row[codeColumn]))
		}
		if _, ok := requested[code]; !ok {
			return nil, databaseError(fmt.Errorf("%w: invitation code query returned an unexpected code", d1.ErrUnavailable))
		}
		return nil, ErrConflict
	}

	for _, code := range codes {
		state.missingInvitationCodes[code] = struct{}{}
	}
	for _, input := range inputs {
		created, err := p.createInvitation(ctx, input, nil)
		if err != nil {
			return nil, err
		}
		result = append(result, created)
	}
	return result, nil
}

func (p *mutationPlan) consumeInvitation(ctx context.Context, id int, at time.Time) error {
	record, err := p.invitationByID(ctx, id)
	if err != nil {
		return err
	}
	record.invitation.UsedAt = cloneTime(&at)
	p.userState().cacheInvitation(record)
	p.update(invitation.Table, id, map[string]any{invitation.FieldUsedAt: at})
	return nil
}

func (p *mutationPlan) invitations(
	ctx context.Context,
	userID int,
	page model.Page,
) ([]*model.Invitation, int, error) {
	query := p.base.client.Invitation.Query().Where(invitation.IssuerIDEQ(userID)).WithRedeemer().
		Order(ent.Desc(invitation.FieldID)).Limit(page.Limit + 1)
	if page.Before > 0 {
		query.Where(invitation.IDLT(page.Before))
	}
	rows, err := query.All(ctx)
	if err != nil {
		return nil, 0, databaseError(err)
	}
	state := p.userState()
	byID := make(map[int]*plannedInvitation, len(rows)+len(state.invitationsByID))
	for _, row := range rows {
		if record, ok := state.invitationsByID[row.ID]; ok {
			byID[row.ID] = clonePlannedInvitation(record)
			continue
		}
		record := plannedInvitationFromEnt(row)
		state.cacheInvitation(record)
		byID[row.ID] = record
	}
	for id, record := range state.invitationsByID {
		if record.invitation.IssuerID == nil || *record.invitation.IssuerID != userID {
			continue
		}
		if page.Before > 0 && id >= page.Before {
			continue
		}
		byID[id] = clonePlannedInvitation(record)
	}
	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	var next int
	if len(ids) > page.Limit {
		ids = ids[:page.Limit]
		if len(ids) > 0 {
			next = ids[len(ids)-1]
		}
	}
	result := make([]*model.Invitation, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneInvitation(&byID[id].invitation))
	}
	return result, next, nil
}

func plannedInvitationFromEnt(row *ent.Invitation) *plannedInvitation {
	return &plannedInvitation{
		invitation:  *invitationInfo(row),
		purchaseKey: row.PurchaseKey,
	}
}

func (p *userPlan) cacheUser(record *UserRecord) {
	copy := cloneUserRecord(record)
	p.usersByID[copy.ID] = copy
	p.usersByName[copy.Username] = copy
	if copy.InvitationID != nil {
		p.usersByInvitation[*copy.InvitationID] = copy
	}
}

func (p *userPlan) cacheCredential(record *Credential) {
	copy := cloneCredential(record)
	p.credentialsByID[copy.ID] = copy
	p.credentialsByHash[copy.SecretHash] = copy
}

func (p *userPlan) cacheInvitation(record *plannedInvitation) {
	copy := clonePlannedInvitation(record)
	p.invitationsByID[copy.invitation.ID] = copy
	p.invitationsByCode[copy.invitation.Code] = copy
	if copy.invitation.IssuerID != nil {
		p.invitationsByPurchase[invitationPurchaseLookup{
			issuerID: *copy.invitation.IssuerID,
			key:      copy.purchaseKey,
		}] = copy
	}
}

func cloneUserRecord(record *UserRecord) *UserRecord {
	if record == nil {
		return nil
	}
	return &UserRecord{User: *cloneUser(&record.User), PasswordHash: record.PasswordHash}
}

func cloneUser(record *model.User) *model.User {
	if record == nil {
		return nil
	}
	copy := *record
	copy.InviterID = cloneInt(record.InviterID)
	copy.InvitationID = cloneInt(record.InvitationID)
	return &copy
}

func cloneCredential(record *Credential) *Credential {
	if record == nil {
		return nil
	}
	copy := *record
	return &copy
}

func cloneInvitation(record *model.Invitation) *model.Invitation {
	if record == nil {
		return nil
	}
	copy := *record
	copy.IssuerID = cloneInt(record.IssuerID)
	copy.RedeemerID = cloneInt(record.RedeemerID)
	copy.UsedAt = cloneTime(record.UsedAt)
	return &copy
}

func clonePlannedInvitation(record *plannedInvitation) *plannedInvitation {
	if record == nil {
		return nil
	}
	return &plannedInvitation{
		invitation:  *cloneInvitation(&record.invitation),
		purchaseKey: record.purchaseKey,
	}
}

func cloneToken(token *model.Token) *model.Token {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
