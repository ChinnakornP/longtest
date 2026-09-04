package org_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/auth/authtest"
)

func TestCreateOrganization(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	// A user who belongs to nothing yet: creating an org is how they start.
	client := env.SignIn(t, env.NewUser(t), uuid.Nil, "")

	var created orgView
	client.Post(t, "/api/v1/orgs", map[string]string{"name": "Second Org"}).
		ExpectStatus(t, http.StatusCreated).JSON(t, &created)

	if created.Name != "Second Org" {
		t.Fatalf("name: got %q", created.Name)
	}
	if created.Slug != "second-org" {
		t.Fatalf("slug: got %q, want %q", created.Slug, "second-org")
	}

	// The creator is the owner, and can immediately act in the new org.
	var members membersView
	client.AsOrg(created.ID).Get(t, membersPath(created.ID)).
		ExpectStatus(t, http.StatusOK).JSON(t, &members)

	if len(members.Members) != 1 {
		t.Fatalf("got %d members, want 1", len(members.Members))
	}
	if members.Members[0].Role != auth.RoleOwner {
		t.Fatalf("role: got %q, want owner", members.Members[0].Role)
	}
	if members.Members[0].UserID != client.UserID {
		t.Fatalf("member: got %s, want %s", members.Members[0].UserID, client.UserID)
	}
}

func TestCreateOrganizationValidation(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	client := env.NewOrg(t)

	tests := []struct {
		name       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"empty name", map[string]string{"name": ""}, http.StatusUnprocessableEntity, "validation_failed"},
		{"whitespace name", map[string]string{"name": "   "}, http.StatusUnprocessableEntity, "validation_failed"},
		{"name too long", map[string]string{"name": strings.Repeat("n", 201)}, http.StatusUnprocessableEntity, "validation_failed"},
		{"unknown field", map[string]string{"title": "x"}, http.StatusUnprocessableEntity, "validation_failed"},
		{"malformed json", `{"name":`, http.StatusBadRequest, "bad_request"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client.Post(t, "/api/v1/orgs", tt.body).ExpectError(t, tt.wantStatus, tt.wantCode)
		})
	}

	env.Anonymous(t).Post(t, "/api/v1/orgs", map[string]string{"name": "Nope"}).
		ExpectError(t, http.StatusUnauthorized, "unauthorized")
}

func TestListMembers(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)

	// Reading the member list is allowed for every role, viewer included.
	for _, client := range []*authtest.Client{owner, viewer} {
		var members membersView
		client.Get(t, membersPath(owner.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &members)

		if len(members.Members) != 2 {
			t.Fatalf("got %d members, want 2", len(members.Members))
		}
		byUser := map[uuid.UUID]auth.Role{}
		for _, m := range members.Members {
			byUser[m.UserID] = m.Role
		}
		if byUser[owner.UserID] != auth.RoleOwner {
			t.Fatalf("owner role: got %q", byUser[owner.UserID])
		}
		if byUser[viewer.UserID] != auth.RoleViewer {
			t.Fatalf("viewer role: got %q", byUser[viewer.UserID])
		}
	}
}

// Acceptance criterion 3: a caller from org A gets 403/404 - never 200 - for
// org B's resources, on every org-scoped endpoint.
func TestCrossTenantAccessIsRefused(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	orgA := env.NewOrg(t)
	orgB := env.NewOrg(t)

	// Something real to reach for in org B, so a 404 cannot be "nothing there
	// anyway".
	var bInvite inviteView
	orgB.Post(t, invitesPath(orgB.OrgID), map[string]string{
		"email": "in-b-" + uuid.NewString() + "@example.test", "role": "member",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &bInvite)

	endpoints := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"members", http.MethodGet, membersPath(orgB.OrgID), nil},
		{"list invites", http.MethodGet, invitesPath(orgB.OrgID), nil},
		{"create invite", http.MethodPost, invitesPath(orgB.OrgID),
			map[string]string{"email": "x@example.test", "role": "member"}},
		{"revoke invite", http.MethodDelete, invitesPath(orgB.OrgID) + "/" + bInvite.ID.String(), nil},
		{"create pairing code", http.MethodPost, pairPath(orgB.OrgID), nil},
	}

	for _, ep := range endpoints {
		// Two ways to try to reach another tenant: claim their org in the
		// header, or keep your own header and put their id in the path.
		t.Run(ep.name+"/org id in the header", func(t *testing.T) {
			orgA.AsOrg(orgB.OrgID).Do(t, ep.method, ep.path, ep.body).
				ExpectError(t, http.StatusForbidden, "forbidden")
		})
		t.Run(ep.name+"/org id in the path", func(t *testing.T) {
			orgA.Do(t, ep.method, ep.path, ep.body).
				ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}

	// Org B's own invite is untouched by all of that.
	var invites invitesView
	orgB.Get(t, invitesPath(orgB.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &invites)
	if len(invites.Invites) != 1 || invites.Invites[0].ID != bInvite.ID {
		t.Fatalf("org B's invite was affected by org A's requests: %+v", invites.Invites)
	}
}

// A resource id from another organization must not be reachable even when the
// caller's own header and path are consistent: the query is org-scoped, so it
// simply is not there.
func TestForeignResourceIdIsNotFound(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	orgA := env.NewOrg(t)
	orgB := env.NewOrg(t)

	var bInvite inviteView
	orgB.Post(t, invitesPath(orgB.OrgID), map[string]string{
		"email": "in-b-" + uuid.NewString() + "@example.test", "role": "viewer",
	}).ExpectStatus(t, http.StatusCreated).JSON(t, &bInvite)

	// Org A, acting correctly in org A, asks to revoke org B's invite by id.
	orgA.Delete(t, invitesPath(orgA.OrgID)+"/"+bInvite.ID.String()).
		ExpectError(t, http.StatusNotFound, "not_found")

	// And it really is still live in org B.
	var invites invitesView
	orgB.Get(t, invitesPath(orgB.OrgID)).ExpectStatus(t, http.StatusOK).JSON(t, &invites)
	if len(invites.Invites) != 1 {
		t.Fatalf("org A revoked org B's invite: %+v", invites.Invites)
	}
}

// Acceptance criterion 6, at the endpoint level rather than the middleware
// level: a viewer may read but not write.
func TestViewerCannotWrite(t *testing.T) {
	env := authtest.New(t, newAPI(t))
	owner := env.NewOrg(t)
	viewer := env.NewMember(t, owner.OrgID, auth.RoleViewer)
	member := env.NewMember(t, owner.OrgID, auth.RoleMember)

	writes := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"create invite", http.MethodPost, invitesPath(owner.OrgID),
			map[string]string{"email": "new-" + uuid.NewString() + "@example.test", "role": "viewer"}},
		{"list invites", http.MethodGet, invitesPath(owner.OrgID), nil},
		{"create pairing code", http.MethodPost, pairPath(owner.OrgID), nil},
	}

	for _, w := range writes {
		t.Run(w.name+"/viewer", func(t *testing.T) {
			viewer.Do(t, w.method, w.path, w.body).ExpectError(t, http.StatusForbidden, "forbidden")
		})
		// A member is allowed to run tests but not to administer the org, so
		// these are refused for them too.
		t.Run(w.name+"/member", func(t *testing.T) {
			member.Do(t, w.method, w.path, w.body).ExpectError(t, http.StatusForbidden, "forbidden")
		})
	}

	// The viewer can still read.
	viewer.Get(t, membersPath(owner.OrgID)).ExpectStatus(t, http.StatusOK)
}
