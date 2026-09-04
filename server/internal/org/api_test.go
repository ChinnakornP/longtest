// Package org_test exercises the organization, invite and pairing endpoints
// against a real Postgres.
package org_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/internal/org"
)

func TestMain(m *testing.M) { authtest.Main(m) }

func newAPI(t *testing.T) http.Handler {
	t.Helper()

	store := authtest.Store(t)
	sessions := auth.NewSessions(store, authtest.SessionConfig())
	hasher := auth.NewHasher(auth.FastPasswordParams())

	orgService := org.NewService(store)
	authService := auth.NewService(store, hasher, sessions, orgService)

	mux := http.NewServeMux()
	auth.NewHandler(authService, sessions).Mount(mux)
	org.NewHandler(orgService, store, sessions).Mount(mux)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return httpx.Chain(mux, httpx.RequestID(logger), httpx.Recover(), httpx.AccessLog())
}

// Response shapes shared by the tests in this package.

type orgView struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

type memberView struct {
	UserID uuid.UUID `json:"userId"`
	Email  string    `json:"email"`
	Name   string    `json:"name"`
	Role   auth.Role `json:"role"`
}

type membersView struct {
	Members []memberView `json:"members"`
}

type inviteView struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Role      auth.Role `json:"role"`
	ExpiresAt time.Time `json:"expiresAt"`
	Token     string    `json:"token"`
}

type invitesView struct {
	Invites []inviteView `json:"invites"`
}

type pairingView struct {
	PairingCode string    `json:"pairingCode"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type redeemView struct {
	RuntimeID    uuid.UUID `json:"runtimeId"`
	RuntimeToken string    `json:"runtimeToken"`
	RuntimeName  string    `json:"runtimeName"`
	OrgID        uuid.UUID `json:"orgId"`
}

// membersPath and friends keep the URL construction in one place: a test that
// builds the wrong URL passes for the wrong reason.
func membersPath(orgID uuid.UUID) string { return "/api/v1/orgs/" + orgID.String() + "/members" }
func invitesPath(orgID uuid.UUID) string { return "/api/v1/orgs/" + orgID.String() + "/invites" }
func pairPath(orgID uuid.UUID) string {
	return "/api/v1/orgs/" + orgID.String() + "/runtimes/pair"
}

// intervalOf builds the pgtype.Interval that ListRuntimes takes for its
// "online within" window.
func intervalOf(t *testing.T, d time.Duration) pgtype.Interval {
	t.Helper()
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
