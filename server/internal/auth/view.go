package auth

import (
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// The JSON shapes below are the API's response contract. They live in this
// package because every API package already depends on it, and because a
// single definition is the only way `me`, `POST /orgs` and the member list
// cannot drift into three slightly different organization objects.
//
// Two rules hold for every view type here:
//
//  1. It is built from a dbgen row by an explicit constructor. Serialising a
//     dbgen struct directly would put `password_hash` and `token_hash` on the
//     wire the first time somebody added a field.
//  2. Timestamps are RFC 3339 in UTC, matching packages/qa-schema.

// UserView is the caller's own account. It is never returned for another
// user; MemberView is the shape for that.
type UserView struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// NewUserView projects a user row. Note what it drops: PasswordHash.
func NewUserView(u dbgen.User) UserView {
	return UserView{
		ID:        u.ID,
		Email:     u.Email,
		Name:      u.Name,
		CreatedAt: u.CreatedAt.Time.UTC(),
	}
}

// OrgView is an organization as the API returns it.
type OrgView struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
}

// NewOrgView projects an organization row.
func NewOrgView(o dbgen.Organization) OrgView {
	return OrgView{
		ID:        o.ID,
		Name:      o.Name,
		Slug:      o.Slug,
		CreatedAt: o.CreatedAt.Time.UTC(),
	}
}

// MembershipView is one entry of the org switcher: an organization flattened
// together with the caller's role in it, which is the shape the contract for
// GET /api/v1/me specifies.
type MembershipView struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
	Role Role      `json:"role"`
}

// NewMembershipViews projects the org list of a user.
func NewMembershipViews(memberships []Membership) []MembershipView {
	out := make([]MembershipView, 0, len(memberships))
	for _, m := range memberships {
		out = append(out, MembershipView{
			ID:   m.Org.ID,
			Name: m.Org.Name,
			Slug: m.Org.Slug,
			Role: m.Role,
		})
	}
	return out
}
