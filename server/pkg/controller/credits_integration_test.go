//go:build integration

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/41tair/slopless/pkg/model"
)

func readCredits(t *testing.T, handler http.Handler, cookie *http.Cookie) model.Credits {
	t.Helper()
	response := perform(handler, testRequest{method: "GET", path: "/api/v1/credits", cookie: cookie})
	requireStatus(t, response, 200)
	var body struct {
		Data model.Credits `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func purchaseCode(t *testing.T, handler http.Handler, request testRequest) model.Invitation {
	t.Helper()
	response := perform(handler, request)
	requireStatus(t, response, 201)
	var body struct {
		Data model.Invitation `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Data
}

func markBatch(t *testing.T, handler http.Handler, request testRequest) {
	t.Helper()
	request.path = "/openapi/v1/accounts/mark"
	requireStatus(t, perform(handler, request), 200)
}

func TestCreditsInvitationAndPropagationHTTPFlow(t *testing.T) {
	t.Parallel()
	_, handler := fixture(t)
	alice := registerLogin(t, handler, "alice")
	witness := registerLogin(t, handler, "witness")
	aliceToken, witnessToken := issueToken(t, handler, alice), issueToken(t, handler, witness)
	if credits := readCredits(t, handler, alice); credits.Balance != 0 || len(credits.Entries) != 0 {
		t.Fatalf("registration minted credits: %+v", credits)
	}
	earnInvitation := func(cookie *http.Cookie, token issuedToken, prefix string) {
		accounts := make([]model.Account, 10)
		for i := range accounts {
			accounts[i] = model.Account{Platform: "github", Username: fmt.Sprintf("%s-%d", prefix, i)}
		}
		body, err := json.Marshal(map[string]any{"accounts": accounts, "marked": true})
		if err != nil {
			t.Fatal(err)
		}
		markBatch(t, handler, testRequest{token: token.Secret, key: prefix + "-first-mark-key", body: string(body)})
		if credits := readCredits(t, handler, cookie); credits.Balance != 0 {
			t.Fatal("first mark alone earned credits")
		}
		markBatch(t, handler, testRequest{token: witnessToken.Secret, key: prefix + "-witness-confirm", body: string(body)})
		if credits := readCredits(t, handler, cookie); credits.Balance != 100 {
			t.Fatalf("credits = %+v", credits)
		}
	}
	earnInvitation(alice, aliceToken, "alice")
	purchase := testRequest{path: "/api/v1/invitations", cookie: alice, key: "alice-invite-key-123", body: `{}`}
	aliceInvite := purchaseCode(t, handler, purchase)
	replayed := purchaseCode(t, handler, purchase)
	if aliceInvite.Code != replayed.Code || aliceInvite.ID != replayed.ID {
		t.Fatal("purchase retry issued another code")
	}
	if credits := readCredits(t, handler, alice); credits.Balance != 0 || credits.Entries[0].Amount != -100 {
		t.Fatalf("incorrect invitation debit: %+v", credits)
	}
	bob := registerWithInvitation(t, handler, fmt.Sprintf(
		`{"username":"bob","password":"a-test-password","invitationCode":%q}`, aliceInvite.Code,
	))
	bobToken := issueToken(t, handler, bob)
	earnInvitation(bob, bobToken, "bob")
	bobInvite := purchaseCode(t, handler, testRequest{
		path: "/api/v1/invitations", cookie: bob, key: "bob-invite-key-123", body: `{}`,
	})
	carol := registerWithInvitation(t, handler, fmt.Sprintf(
		`{"username":"carol","password":"a-test-password","invitationCode":%q}`, bobInvite.Code,
	))
	carolToken := issueToken(t, handler, carol)
	for _, pair := range []struct {
		cookie *http.Cookie
		invite model.Invitation
	}{
		{cookie: bob, invite: aliceInvite}, {cookie: carol, invite: bobInvite},
	} {
		response := perform(handler, testRequest{method: "GET", path: "/api/v1/user", cookie: pair.cookie})
		requireStatus(t, response, 200)
		var body struct {
			Data model.User `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Data.InviterID == nil || *body.Data.InviterID != *pair.invite.IssuerID || *body.Data.InvitationID != pair.invite.ID {
			t.Fatalf("missing invitation relationship: %+v", body.Data)
		}
	}
	markBody := `{"accounts":[{"platform":"github","username":"target-x"}],"marked":true}`
	markBatch(t, handler, testRequest{token: aliceToken.Secret, key: "alice-first-target-x", body: markBody})
	markBatch(t, handler, testRequest{token: carolToken.Secret, key: "carol-confirms-target", body: markBody})
	markBatch(t, handler, testRequest{token: carolToken.Secret, key: "carol-confirms-target", body: markBody})
	if got := readCredits(t, handler, alice); got.Balance != 10 || got.Entries[0].Reason != "first_mark" {
		t.Fatalf("wrong first-marker award: %+v", got)
	}
	if got := readCredits(t, handler, bob); got.Balance != 2 || got.Entries[0].Reason != "invitation_contribution" {
		t.Fatalf("wrong invitation award: %+v", got)
	}
	if got := readCredits(t, handler, carol); got.Balance != 0 {
		t.Fatal("confirmation itself minted credits")
	}
	response := perform(handler, testRequest{method: "GET", path: "/api/v1/invitations", cookie: alice})
	requireStatus(t, response, 200)
	var invites struct {
		Data []model.Invitation `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &invites); err != nil {
		t.Fatal(err)
	}
	if len(invites.Data) != 1 || invites.Data[0].RedeemerID == nil || invites.Data[0].UsedAt == nil {
		t.Fatalf("missing consumption trail: %+v", invites)
	}
	response = perform(handler, testRequest{
		path: "/api/v1/register", body: fmt.Sprintf(
			`{"username":"another","password":"a-test-password","invitationCode":%q}`, aliceInvite.Code,
		),
	})
	requireStatus(t, response, 422)
	requireStatus(t, perform(handler, testRequest{
		path: "/api/v1/register", body: `{"username":"missing-code","password":"a-test-password"}`,
	}), 422)
	requireStatus(t, perform(handler, testRequest{
		path: "/api/v1/invitations", cookie: alice, key: "another-purchase-key", body: `{}`,
	}), 409)
	response = perform(handler, testRequest{method: "GET", path: "/api/v1/credits?limit=1", cookie: alice})
	requireStatus(t, response, 200)
	var credits struct {
		Data model.Credits `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &credits); err != nil {
		t.Fatal(err)
	}
	if len(credits.Data.Entries) != 1 || credits.Data.NextCursor == 0 {
		t.Fatalf("invalid first page: %+v", credits)
	}
	response = perform(handler, testRequest{
		method: "GET", path: fmt.Sprintf("/api/v1/credits?limit=1&before=%d", credits.Data.NextCursor), cookie: alice,
	})
	requireStatus(t, response, 200)
	var next struct {
		Data model.Credits `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next.Data.Entries[0].ID >= credits.Data.Entries[0].ID {
		t.Fatal("pagination repeated a ledger entry")
	}
	for _, request := range []testRequest{
		{method: "GET", path: "/api/v1/credits", token: aliceToken.Secret},
		{method: "GET", path: "/api/v1/invitations", token: aliceToken.Secret},
		{path: "/api/v1/invitations", token: aliceToken.Secret, body: `{}`},
	} {
		requireStatus(t, perform(handler, request), 401)
	}
	requireStatus(t, perform(handler, testRequest{method: "GET", path: "/api/v1/credits?limit=0", cookie: alice}), 422)
	requireStatus(t, perform(handler, testRequest{path: "/api/v1/invitations", cookie: alice, body: `{}`}), 400)
}
