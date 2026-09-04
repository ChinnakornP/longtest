package auth

import (
	"fmt"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// Role is a membership role, ordered by privilege.
//
// The MVP gate, from the task contract:
//
//	viewer  read-only
//	member  + create and run test runs
//	admin   + manage members, invites and runtimes
//	owner   + transfer or delete the organization
//
// The order is total, so authorization is "at least this role" rather than a
// set membership test. That is what makes RequireRole a one-liner on a route
// and keeps the rule out of the handlers.
type Role string

// The four roles, least privileged first.
const (
	RoleViewer Role = "viewer"
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// rank orders the roles. Zero means "not a role", so an unset or unknown value
// never satisfies AtLeast.
func (r Role) rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleMember:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	default:
		return 0
	}
}

// Valid reports whether r is one of the four roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// AtLeast reports whether r is at least as privileged as min.
//
// An invalid r is never sufficient, so a context that was never populated by
// the middleware fails closed.
func (r Role) AtLeast(minimum Role) bool {
	if !r.Valid() || !minimum.Valid() {
		return false
	}
	return r.rank() >= minimum.rank()
}

// String makes Role printable in logs and error messages.
func (r Role) String() string { return string(r) }

// DB converts to the generated enum used by the query layer.
func (r Role) DB() dbgen.MembershipRole { return dbgen.MembershipRole(r) }

// RoleFromDB converts the generated enum back. An unrecognised value yields an
// invalid Role, which fails every AtLeast check rather than defaulting to
// something permissive.
func RoleFromDB(r dbgen.MembershipRole) Role {
	role := Role(r)
	if !role.Valid() {
		return ""
	}
	return role
}

// ParseRole validates a role that arrived over the wire (an invite body).
func ParseRole(raw string) (Role, error) {
	role := Role(raw)
	if !role.Valid() {
		return "", fmt.Errorf("%q is not one of viewer, member, admin, owner", raw)
	}
	return role, nil
}
