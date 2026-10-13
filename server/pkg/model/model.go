// Package model defines values and errors shared by the application layers.
package model

import (
	"errors"
	"time"

	"github.com/41tair/slopless/pkg/config"
)

type User struct {
	ID           int       `json:"id"`
	Username     string    `json:"username"`
	CreatedAt    time.Time `json:"createdAt"`
	Credits      int64     `json:"credits"`
	InviterID    *int      `json:"inviterId"`
	InvitationID *int      `json:"invitationId"`
}

type Token struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Identity struct {
	UserID  int
	TokenID int
}

type Registration struct {
	Username       string
	Password       string
	InvitationCode string
}

type TokenInput struct {
	UserID    int
	Name      string
	ExpiresAt *time.Time
}

type Account struct {
	Platform string `json:"platform"`
	Username string `json:"username"`
}

type AccountState struct {
	Account
	Marked     bool  `json:"marked"`
	MarkCount  int64 `json:"markCount"`
	MarkedByMe bool  `json:"markedByMe"`
}

type MarkInput struct {
	Accounts []Account `json:"accounts"`
	Marked   bool      `json:"marked"`
	Key      string    `json:"-"`
}

type CreditEntry struct {
	ID             int       `json:"id"`
	Amount         int64     `json:"amount"`
	BalanceAfter   int64     `json:"balanceAfter"`
	Reason         string    `json:"reason"`
	Account        *Account  `json:"account,omitempty"`
	ConfirmationID *int      `json:"confirmationId,omitempty"`
	ConfirmedByID  *int      `json:"confirmedById,omitempty"`
	InvitationID   *int      `json:"invitationId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Credits struct {
	Balance    int64          `json:"balance"`
	Rules      config.Credits `json:"rules"`
	Entries    []CreditEntry  `json:"entries"`
	NextCursor int            `json:"nextCursor,omitempty"`
}

type Invitation struct {
	ID         int        `json:"id"`
	Code       string     `json:"code"`
	IssuerID   *int       `json:"issuerId"`
	RedeemerID *int       `json:"redeemerId"`
	Cost       int64      `json:"cost"`
	CreatedAt  time.Time  `json:"createdAt"`
	UsedAt     *time.Time `json:"usedAt"`
}

type Page struct {
	Before int
	Limit  int
}

type RatePolicy struct {
	Policy        string `json:"policy"`
	Limit         int    `json:"limit"`
	WindowSeconds int    `json:"windowSeconds"`
	Unit          string `json:"unit"`
	Subject       string `json:"-"`
	Units         int    `json:"-"`
}

type RateLimitError struct {
	RetryAfterSeconds int
	RateLimit         RatePolicy
}

func (e *RateLimitError) Error() string { return "rate limit exceeded: " + e.RateLimit.Policy }

var (
	ErrUnauthorized        = errors.New("invalid credentials")
	ErrUsernameTaken       = errors.New("username already exists")
	ErrNotFound            = errors.New("not found")
	ErrUnavailable         = errors.New("database unavailable")
	ErrIdempotencyConflict = errors.New("idempotency key used with different input")
	ErrInvalidInvitation   = errors.New("invalid or already used invitation")
	ErrInsufficientCredits = errors.New("insufficient credits")
)

type ValidationError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string { return e.Message }
