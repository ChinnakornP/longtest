package org_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

func TestInviteRoundTrip(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	// Somebody with an account but no membership - the ordinary case, since
	// the invite link is what brings them here.
	invitee := env.NewUser(t)
	joiner := env.SignIn(t, invitee, uuid.Nil, "")

	var invite inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": invitee.Email, "role": "member",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &invite)

	if invite.Token == "" {
		t.Fatal("the invite response did not carry a token")
	}
	if invite.Role != auth.RoleMember {
		t.Fatalf("role: got %q, want member", invite.Role)
	}
	if !strings.EqualFold(invite.Email, invitee.Email) {
		t.Fatalf("email: got %q, want %q", invite.Email, invitee.Email)
	}

	// The token is shown once: listing invites must not carry it.
	var listed invitesView
	owner.Get(t, invitesPath(owner.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &listed)
	if len(listed.Invites) != 1 {
		t.Fatalf("got %d invites, want 1", len(listed.Invites))
	}
	if listed.Invites[0].Token != "" {
		t.Fatal("the invite list leaked the token")
	}

	// Accepting joins the organization at the invited role.
	var accepted struct {
		Org  orgView   `json:"org"`
		Role auth.Role `json:"role"`
	}
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectStatus(t, http.StatusOK).JSON(t, &accepted)

	if accepted.Org.ID != owner.OrgID {
		t.Fatalf("organization: got %s, want %s", accepted.Org.ID, owner.OrgID)
	}
	if accepted.Role != auth.RoleMember {
		t.Fatalf("role: got %q, want member", accepted.Role)
	}

	// The membership is real: the joiner can now act in the organization.
	var members membersView
	joiner.AsOrg(owner.OrgID).Get(t, membersPath(owner.OrgID)).
		ExpectStatus(t, http.StatusOK).JSON(t, &members)
	if len(members.Members) != 2 {
		t.Fatalf("got %d members, want 2", len(members.Members))
	}

	// ...but only at the role they were invited to.
	joiner.AsOrg(owner.OrgID).Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": "another-" + uuid.NewString() + "@example.test", "role": "viewer",
	}).ExpectError(t, http.StatusForbidden, "forbidden")

	// The invite is consumed: replaying the token fails.
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectError(t, http.StatusNotFound, "not_found")

	// And it is gone from the outstanding list.
	owner.Get(t, invitesPath(owner.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &listed)
	if len(listed.Invites) != 0 {
		t.Fatalf("an accepted invite is still outstanding: %+v", listed.Invites)
	}
}

// A leaked invite link must be useless to whoever finds it.
func TestInviteIsBoundToItsEmail(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	invitee := env.NewUser(t)
	finder := env.SignIn(t, env.NewUser(t), uuid.Nil, "")

	var invite inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": invitee.Email, "role": "admin",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &invite)

	finder.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectError(t, http.StatusForbidden, "forbidden")

	// The invite is still live for the person it was meant for.
	env.SignIn(t, invitee, uuid.Nil, "").
		Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectStatus(t, http.StatusOK)
}

func TestAcceptInviteRejectsBadTokens(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	joiner := env.SignIn(t, env.NewUser(t), uuid.Nil, "")

	var invite inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": joiner.Email, "role": "viewer",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &invite)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"forged", uuid.NewString()},
		{"truncated", invite.Token[:len(invite.Token)-1]},
		{"one character changed", flipLast(invite.Token)},
		{"absurdly long", strings.Repeat("t", 600)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": tt.token}).
				ExpectError(t, http.StatusNotFound, "not_found")
		})
	}

	// The real token still works, so nothing above consumed it.
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectStatus(t, http.StatusOK)
}

func TestRevokedInviteCannotBeAccepted(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	joiner := env.SignIn(t, env.NewUser(t), uuid.Nil, "")

	var invite inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": joiner.Email, "role": "member",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &invite)

	owner.Delete(t, invitesPath(owner.OrgID)+"/"+invite.ID.String()).
		ExpectStatus(t, http.StatusNoContent)

	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": invite.Token}).
		ExpectError(t, http.StatusNotFound, "not_found")

	// Revoking twice is a 404, not a silent success: the second caller should
	// know nothing changed.
	owner.Delete(t, invitesPath(owner.OrgID)+"/"+invite.ID.String()).
		ExpectError(t, http.StatusNotFound, "not_found")
}

// Re-inviting the same address rotates the token instead of leaving two live
// invites, which is what the partial unique index in migration 00010 enforces.
func TestReInvitingRotatesTheToken(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	joiner := env.SignIn(t, env.NewUser(t), uuid.Nil, "")

	var first, second inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": joiner.Email, "role": "viewer",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &first)

	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": joiner.Email, "role": "member",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &second)

	if first.Token == second.Token {
		t.Fatal("re-inviting reused the token")
	}

	var listed invitesView
	owner.Get(t, invitesPath(owner.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &listed)
	if len(listed.Invites) != 1 {
		t.Fatalf("got %d live invites, want exactly 1", len(listed.Invites))
	}

	// The superseded token is dead...
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": first.Token}).
		ExpectError(t, http.StatusNotFound, "not_found")
	// ...and the new one carries the new role.
	var accepted struct {
		Role auth.Role `json:"role"`
	}
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": second.Token}).
		ExpectStatus(t, http.StatusOK).JSON(t, &accepted)
	if accepted.Role != auth.RoleMember {
		t.Fatalf("role: got %q, want member", accepted.Role)
	}
}

// Nobody may hand out a role above their own, or "admin" would be a synonym
// for "owner" one invite later.
func TestInviteCannotEscalatePrivilege(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	admin := env.NewMember(t, owner.OrgID, auth.RoleAdmin)

	admin.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": "would-be-owner-" + uuid.NewString() + "@example.test", "role": "owner",
	}).ExpectError(t, http.StatusForbidden, "forbidden")

	// An admin may still invite up to their own level.
	for _, role := range []string{"admin", "member", "viewer"} {
		admin.Post(t, invitesPath(owner.OrgID), map[string]string{
			"email": role + "-" + uuid.NewString() + "@example.test", "role": role,
		}).ExpectStatus(t, http.StatusCreated)
	}

	// And an owner may hand over ownership.
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": "new-owner-" + uuid.NewString() + "@example.test", "role": "owner",
	}).ExpectStatus(t, http.StatusCreated)
}

func TestInviteValidation(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"missing email", map[string]string{"email": "", "role": "member"},
			http.StatusUnprocessableEntity, "validation_failed"},
		{"malformed email", map[string]string{"email": "not-an-email", "role": "member"},
			http.StatusUnprocessableEntity, "validation_failed"},
		{"unknown role", map[string]string{"email": "a@example.test", "role": "superuser"},
			http.StatusUnprocessableEntity, "validation_failed"},
		{"empty role", map[string]string{"email": "a@example.test", "role": ""},
			http.StatusUnprocessableEntity, "validation_failed"},
		{"unknown field", map[string]string{"email": "a@example.test", "role": "member", "expires": "never"},
			http.StatusUnprocessableEntity, "validation_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner.Post(t, invitesPath(owner.OrgID), tt.body).ExpectError(t, tt.wantStatus, tt.wantCode)
		})
	}
}

// Inviting somebody who is already in the organization would silently change
// their role on accept, so it is refused as a conflict instead.
func TestInvitingAnExistingMemberIsAConflict(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	member := env.NewMember(t, owner.OrgID, auth.RoleMember)

	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": member.Email, "role": "admin",
	}).ExpectError(t, http.StatusConflict, "conflict")
}

// Accepting a weaker invite must not demote somebody: an admin who clicks an
// old "you are invited as a viewer" link stays an admin.
func TestAcceptingAWeakerInviteDoesNotDowngrade(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)

	joinerUser := env.NewUser(t)
	joiner := env.SignIn(t, joinerUser, uuid.Nil, "")

	var viewerInvite inviteView
	owner.Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": joinerUser.Email, "role": "viewer",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &viewerInvite)

	// They are promoted to admin by another route before the link is clicked.
	promoteToAdmin(t, env, owner.OrgID, joinerUser.ID)

	var accepted struct {
		Role auth.Role `json:"role"`
	}
	joiner.Post(t, "/api/v1/invites/accept", map[string]string{"token": viewerInvite.Token}).
		ExpectStatus(t, http.StatusOK).JSON(t, &accepted)

	if accepted.Role != auth.RoleAdmin {
		t.Fatalf("role after accepting a viewer invite: got %q, want admin", accepted.Role)
	}
	// The role in the database matches what was reported.
	joiner.AsOrg(owner.OrgID).Post(t, invitesPath(owner.OrgID), map[string]string{
		"email": "still-admin-" + uuid.NewString() + "@example.test", "role": "viewer",
	}).ExpectStatus(t, http.StatusCreated)
}

func promoteToAdmin(t *testing.T, env *authtest.Env, orgID, userID uuid.UUID) {
	t.Helper()
	if _, err := env.Store.UpsertMembership(t.Context(), dbgen.UpsertMembershipParams{
		OrgID: orgID, UserID: userID, Role: auth.RoleAdmin.DB(),
	}); err != nil {
		t.Fatalf("promote to admin: %v", err)
	}
}

func flipLast(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	replacement := byte('a')
	if last == 'a' {
		replacement = 'b'
	}
	return s[:len(s)-1] + string(replacement)
}
