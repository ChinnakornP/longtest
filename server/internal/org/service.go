package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Durations that are policy rather than configuration. They are short on
// purpose: both are bearer credentials that travel out of band.
const (
	// PairingCodeTTL is fixed by the task contract at 15 minutes.
	PairingCodeTTL = 15 * time.Minute
	// InviteTTL bounds how long an invite link stays usable.
	InviteTTL = 7 * 24 * time.Hour
	// MaxRuntimeNameLength matches the CHECK on runtimes.name.
	MaxRuntimeNameLength = 200
)

// Service owns organizations, memberships, invites and runtime pairing.
//
// Every authorization decision in this file is made here, not in the handler:
// the handler's middleware has already established that the caller is a member
// with some role, but "an admin may not mint an owner" and "an invite may only
// be accepted by the address it was issued to" are business rules and belong
// next to the transaction that acts on them.
type Service struct {
	store auth.Store
}

// NewService wires the org service.
func NewService(store auth.Store) *Service { return &Service{store: store} }

// CreateForOwner creates an organization and makes ownerID its owner.
//
// It takes the caller's *dbgen.Queries so signup can create the user, the
// organization, the membership and the session in ONE transaction. This is the
// auth.OrgCreator implementation.
func (s *Service) CreateForOwner(
	ctx context.Context, q *dbgen.Queries, ownerID uuid.UUID, name string,
) (dbgen.Organization, error) {
	slug, err := availableSlug(ctx, q, name)
	if err != nil {
		return dbgen.Organization{}, err
	}

	org, err := q.CreateOrganization(ctx, dbgen.CreateOrganizationParams{Name: name, Slug: slug})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrConflict) {
			return dbgen.Organization{}, httpx.Conflict("that organization name is taken, try again")
		}
		return dbgen.Organization{}, fmt.Errorf("create organization: %w", db.Classify(err))
	}

	if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
		OrgID:  org.ID,
		UserID: ownerID,
		Role:   auth.RoleOwner.DB(),
	}); err != nil {
		return dbgen.Organization{}, fmt.Errorf("grant owner membership: %w", db.Classify(err))
	}
	return org, nil
}

// Create is POST /api/v1/orgs: any signed-in user may start a new
// organization, and becomes its owner.
func (s *Service) Create(ctx context.Context, caller auth.Caller, name string) (dbgen.Organization, error) {
	trimmed, err := validateName(name, "name")
	if err != nil {
		return dbgen.Organization{}, err
	}

	var org dbgen.Organization
	// Two rows (organization + membership): one transaction. An org with no
	// owner cannot be administered by anyone, ever.
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		var txErr error
		org, txErr = s.CreateForOwner(ctx, q, caller.UserID(), trimmed)
		return txErr
	})
	if err != nil {
		return dbgen.Organization{}, err
	}
	return org, nil
}

// Member is one row of the member list.
type Member struct {
	UserID   uuid.UUID
	Email    string
	Name     string
	Role     auth.Role
	JoinedAt time.Time
}

// ListMembers returns everyone in the caller's active organization.
//
// The scope comes from the middleware, so the org id can only be one the
// caller is a member of - there is no argument here to pass the wrong org in.
func (s *Service) ListMembers(ctx context.Context, scope auth.OrgScope) ([]Member, error) {
	rows, err := s.store.ListMembers(ctx, scope.OrgID())
	if err != nil {
		return nil, fmt.Errorf("list members: %w", db.Classify(err))
	}

	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		out = append(out, Member{
			UserID:   row.UserID,
			Email:    row.Email,
			Name:     row.Name,
			Role:     auth.RoleFromDB(row.Role),
			JoinedAt: row.CreatedAt.Time.UTC(),
		})
	}
	return out, nil
}

// CreateInvite issues an invite and returns it together with the one-time
// token. The token is the only copy: it is not stored and cannot be shown
// again.
//
// Re-inviting an address rotates the token rather than leaving two live
// invites for the same person, which is why the revoke and the insert share a
// transaction.
func (s *Service) CreateInvite(
	ctx context.Context, scope auth.OrgScope, email string, role auth.Role,
) (dbgen.Invite, string, error) {
	normalisedEmail, err := auth.NormalizeEmail(email)
	if err != nil {
		return dbgen.Invite{}, "", err
	}
	if !role.Valid() {
		return dbgen.Invite{}, "", httpx.InvalidField("role", "must be one of viewer, member, admin, owner")
	}
	// Privilege escalation guard: an admin cannot mint an owner, and nobody
	// can invite somebody to a role above their own.
	if !scope.Role().AtLeast(role) {
		return dbgen.Invite{}, "", httpx.Forbidden("you cannot invite somebody as %s; your own role is %s", role, scope.Role())
	}
	if err := s.rejectExistingMember(ctx, scope.OrgID(), normalisedEmail); err != nil {
		return dbgen.Invite{}, "", err
	}

	token, err := auth.NewInviteToken()
	if err != nil {
		return dbgen.Invite{}, "", err
	}
	tokenHash, err := auth.HashBearerToken(token)
	if err != nil {
		return dbgen.Invite{}, "", err
	}

	var invite dbgen.Invite
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		if _, err := q.RevokeLiveInvitesForEmail(ctx, dbgen.RevokeLiveInvitesForEmailParams{
			OrgID: scope.OrgID(),
			Email: normalisedEmail,
		}); err != nil {
			return fmt.Errorf("revoke previous invites: %w", db.Classify(err))
		}

		created, err := q.CreateInvite(ctx, dbgen.CreateInviteParams{
			OrgID:     scope.OrgID(),
			Email:     normalisedEmail,
			Role:      role.DB(),
			TokenHash: tokenHash,
			InvitedBy: uuid.NullUUID{UUID: scope.UserID(), Valid: true},
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(InviteTTL), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create invite: %w", db.Classify(err))
		}
		invite = created
		return nil
	})
	if err != nil {
		return dbgen.Invite{}, "", err
	}
	return invite, token, nil
}

// rejectExistingMember refuses an invite for somebody who is already in the
// organization. Without it, accepting would silently change their role, which
// is a member-management action with different auditing needs.
func (s *Service) rejectExistingMember(ctx context.Context, orgID uuid.UUID, email string) error {
	user, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return nil // no account yet: they will sign up, then accept
		}
		return fmt.Errorf("look up invitee: %w", db.Classify(err))
	}

	if _, err := s.store.GetMembership(ctx, dbgen.GetMembershipParams{
		OrgID:  orgID,
		UserID: user.ID,
	}); err == nil {
		return httpx.Conflict("that person is already a member of this organization")
	} else if !errors.Is(db.Classify(err), db.ErrNotFound) {
		return fmt.Errorf("look up membership: %w", db.Classify(err))
	}
	return nil
}

// ListInvites returns the organization's outstanding invites. Token hashes are
// never projected out of this function.
func (s *Service) ListInvites(ctx context.Context, scope auth.OrgScope) ([]dbgen.Invite, error) {
	invites, err := s.store.ListInvites(ctx, scope.OrgID())
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", db.Classify(err))
	}
	return invites, nil
}

// RevokeInvite withdraws an outstanding invite.
func (s *Service) RevokeInvite(ctx context.Context, scope auth.OrgScope, inviteID uuid.UUID) error {
	rows, err := s.store.RevokeInvite(ctx, dbgen.RevokeInviteParams{OrgID: scope.OrgID(), ID: inviteID})
	if err != nil {
		return fmt.Errorf("revoke invite: %w", db.Classify(err))
	}
	if rows == 0 {
		// Either no such invite, or it belongs to another organization. The
		// query is org-scoped, so those are indistinguishable here - which is
		// the answer we want to give either way.
		return httpx.NotFound("no such invite")
	}
	return nil
}

// AcceptedInvite is the result of accepting one.
type AcceptedInvite struct {
	Org  dbgen.Organization
	Role auth.Role
}

// AcceptInvite joins the caller to the organization the token names.
//
// Three rules, in this order:
//
//  1. The token must be live. Expiry, revocation and prior acceptance are all
//     filtered in SQL.
//  2. The invite's address must be the caller's. Otherwise a leaked link would
//     let whoever found it into somebody else's organization.
//  3. An existing membership is never downgraded - accepting a viewer invite
//     while already an admin consumes the invite and leaves the admin role.
func (s *Service) AcceptInvite(ctx context.Context, caller auth.Caller, token string) (AcceptedInvite, error) {
	tokenHash, err := auth.HashBearerToken(token)
	if err != nil {
		return AcceptedInvite{}, httpx.NotFound("that invite is not valid or has expired")
	}

	invite, err := s.store.GetLiveInviteByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return AcceptedInvite{}, httpx.NotFound("that invite is not valid or has expired")
		}
		return AcceptedInvite{}, fmt.Errorf("look up invite: %w", db.Classify(err))
	}
	if !strings.EqualFold(invite.Email, caller.Email()) {
		return AcceptedInvite{}, httpx.Forbidden("that invite was issued to a different e-mail address")
	}

	role := auth.RoleFromDB(invite.Role)
	if !role.Valid() {
		return AcceptedInvite{}, fmt.Errorf("invite %s carries an unusable role", invite.ID)
	}

	var (
		org         dbgen.Organization
		grantedRole = role
	)
	// Claim the invite and grant the membership together: an invite marked
	// accepted without a membership row is unrecoverable, and a membership
	// granted from an invite that stays live is a reusable invite.
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		claimed, err := q.AcceptInvite(ctx, dbgen.AcceptInviteParams{
			TokenHash:  tokenHash,
			AcceptedBy: uuid.NullUUID{UUID: caller.UserID(), Valid: true},
		})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				// Somebody (or another tab) claimed it between the read above
				// and this UPDATE.
				return httpx.Conflict("that invite has already been used")
			}
			return fmt.Errorf("accept invite: %w", db.Classify(err))
		}

		if existing, err := q.GetMembership(ctx, dbgen.GetMembershipParams{
			OrgID:  claimed.OrgID,
			UserID: caller.UserID(),
		}); err == nil {
			if current := auth.RoleFromDB(existing.Role); current.AtLeast(grantedRole) {
				grantedRole = current
			}
		} else if !errors.Is(db.Classify(err), db.ErrNotFound) {
			return fmt.Errorf("look up membership: %w", db.Classify(err))
		}

		if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
			OrgID:  claimed.OrgID,
			UserID: caller.UserID(),
			Role:   grantedRole.DB(),
		}); err != nil {
			return fmt.Errorf("grant membership: %w", db.Classify(err))
		}

		org, err = q.GetOrganization(ctx, claimed.OrgID)
		if err != nil {
			return fmt.Errorf("load organization: %w", db.Classify(err))
		}
		return nil
	})
	if err != nil {
		return AcceptedInvite{}, err
	}
	return AcceptedInvite{Org: org, Role: grantedRole}, nil
}

// PairingCode is a one-time daemon pairing code, returned to the operator who
// created it.
type PairingCode struct {
	Code      string
	ExpiresAt time.Time
}

// CreatePairingCode issues a one-time code an operator types into
// `qa-agent pair`. Only the SHA-256 of the code is stored.
func (s *Service) CreatePairingCode(ctx context.Context, scope auth.OrgScope) (PairingCode, error) {
	code, hash, err := auth.NewPairingCode()
	if err != nil {
		return PairingCode{}, err
	}
	expiresAt := time.Now().Add(PairingCodeTTL)

	if _, err := s.store.CreatePairingCode(ctx, dbgen.CreatePairingCodeParams{
		OrgID:     scope.OrgID(),
		CodeHash:  hash,
		CreatedBy: uuid.NullUUID{UUID: scope.UserID(), Valid: true},
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return PairingCode{}, fmt.Errorf("create pairing code: %w", db.Classify(err))
	}
	return PairingCode{Code: code, ExpiresAt: expiresAt}, nil
}

// HostInfo is what a daemon reports about the machine it runs on. It is
// rendered in the runtime list and never interpreted.
type HostInfo struct {
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Version  string `json:"version,omitempty"`
}

// RedeemInput is the POST /api/v1/runtimes/redeem body.
type RedeemInput struct {
	PairingCode string
	RuntimeName string
	HostInfo    HostInfo
}

// RedeemedRuntime is what the daemon stores after pairing.
type RedeemedRuntime struct {
	Runtime dbgen.Runtime
	// Token is shown exactly once. It is not recoverable afterwards; a daemon
	// that loses it has to be paired again.
	Token string
}

// RedeemPairingCode exchanges a one-time code for a runtime and its token.
//
// This endpoint is unauthenticated by necessity - a fresh daemon has no
// credential yet - so everything it trusts comes from the code:
//
//   - the ORGANIZATION comes from the pairing_codes row, never from the body;
//   - single use is enforced by ConsumePairingCode's `consumed_at IS NULL`
//     predicate, which is an atomic claim rather than a read-then-write. Two
//     daemons racing on one code produce one winner and one rolled-back
//     transaction, including the runtime row the loser had already inserted;
//   - expiry is enforced in SQL, so a clock difference in Go cannot extend it.
func (s *Service) RedeemPairingCode(ctx context.Context, in RedeemInput) (RedeemedRuntime, error) {
	codeHash, err := auth.HashPairingCode(in.PairingCode)
	if err != nil {
		// Not even shaped like a code. Same answer as an unknown one.
		return RedeemedRuntime{}, errUnknownPairingCode()
	}
	runtimeName, err := validateRuntimeName(in.RuntimeName)
	if err != nil {
		return RedeemedRuntime{}, err
	}
	hostInfo, err := marshalHostInfo(in.HostInfo)
	if err != nil {
		return RedeemedRuntime{}, err
	}

	token, tokenHash, err := auth.NewRuntimeToken()
	if err != nil {
		return RedeemedRuntime{}, err
	}

	var runtime dbgen.Runtime
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		// Read first, because the runtime row has to exist before the code can
		// be consumed against it (pairing_codes.runtime_id is a composite FK).
		// The claim below is still what decides the winner.
		code, err := q.GetLivePairingCodeByHash(ctx, codeHash)
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				return errUnknownPairingCode()
			}
			return fmt.Errorf("look up pairing code: %w", db.Classify(err))
		}

		created, err := q.CreateRuntime(ctx, dbgen.CreateRuntimeParams{
			OrgID:    code.OrgID,
			Name:     runtimeName,
			Version:  in.HostInfo.Version,
			HostInfo: hostInfo,
		})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrConflict) {
				return httpx.Conflict("a runtime called %q already exists in this organization", runtimeName)
			}
			return fmt.Errorf("create runtime: %w", db.Classify(err))
		}

		if _, err := q.ConsumePairingCode(ctx, dbgen.ConsumePairingCodeParams{
			CodeHash:  codeHash,
			RuntimeID: uuid.NullUUID{UUID: created.ID, Valid: true},
		}); err != nil {
			if errors.Is(db.Classify(err), db.ErrNotFound) {
				// Consumed or expired between the read and here. Rolling back
				// takes the runtime row with it.
				return errUnknownPairingCode()
			}
			return fmt.Errorf("consume pairing code: %w", db.Classify(err))
		}

		if _, err := q.CreateRuntimeToken(ctx, dbgen.CreateRuntimeTokenParams{
			OrgID:     code.OrgID,
			RuntimeID: created.ID,
			TokenHash: tokenHash,
		}); err != nil {
			return fmt.Errorf("create runtime token: %w", db.Classify(err))
		}

		runtime = created
		return nil
	})
	if err != nil {
		return RedeemedRuntime{}, err
	}
	return RedeemedRuntime{Runtime: runtime, Token: token}, nil
}

// errUnknownPairingCode is one message for "no such code", "already used" and
// "expired": a caller with a wrong code learns nothing about which.
func errUnknownPairingCode() error {
	return httpx.NotFound("that pairing code is not valid, has expired, or has already been used")
}

func validateName(raw, field string) (string, error) {
	const maxLen = 200
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return "", httpx.InvalidField(field, "is required")
	case len([]rune(trimmed)) > maxLen:
		return "", httpx.InvalidField(field, fmt.Sprintf("must be at most %d characters", maxLen))
	default:
		return trimmed, nil
	}
}

func validateRuntimeName(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", httpx.InvalidField("runtimeName", "is required")
	}
	if len([]rune(trimmed)) > MaxRuntimeNameLength {
		return "", httpx.InvalidField("runtimeName",
			fmt.Sprintf("must be at most %d characters", MaxRuntimeNameLength))
	}
	// The name is rendered in the dashboard and written to logs; control
	// characters in it are never legitimate.
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", httpx.InvalidField("runtimeName", "must not contain control characters")
		}
	}
	return trimmed, nil
}

// marshalHostInfo validates and encodes the machine facts a daemon reports.
//
// The values are bounded and the OS is checked against the same vocabulary as
// the `hello` frame in packages/qa-schema, so the column cannot accumulate a
// second spelling of "darwin" that the UI then has to know about.
func marshalHostInfo(in HostInfo) ([]byte, error) {
	const maxFieldLength = 255

	fields := map[string]string{
		"hostname": in.Hostname,
		"os":       in.OS,
		"arch":     in.Arch,
		"version":  in.Version,
	}
	for name, value := range fields {
		if len([]rune(value)) > maxFieldLength {
			return nil, httpx.InvalidField("hostInfo."+name,
				fmt.Sprintf("must be at most %d characters", maxFieldLength))
		}
		for _, r := range value {
			if r < 0x20 || r == 0x7f {
				return nil, httpx.InvalidField("hostInfo."+name, "must not contain control characters")
			}
		}
	}

	if in.OS != "" && !validOS[in.OS] {
		return nil, httpx.InvalidField("hostInfo.os", "must be one of linux, darwin, windows")
	}

	encoded, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode host info: %w", err)
	}
	return encoded, nil
}

// validOS mirrors the `os` enum of HelloPayload in packages/qa-schema.
var validOS = map[string]bool{"linux": true, "darwin": true, "windows": true}
