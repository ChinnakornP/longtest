package auth

import (
	"testing"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

func TestRoleAtLeast(t *testing.T) {
	t.Parallel()

	// The full matrix, because this table IS the authorization model: a wrong
	// cell here is a privilege escalation, not a style problem.
	tests := []struct {
		role    Role
		minimum Role
		want    bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleMember, false},
		{RoleViewer, RoleAdmin, false},
		{RoleViewer, RoleOwner, false},

		{RoleMember, RoleViewer, true},
		{RoleMember, RoleMember, true},
		{RoleMember, RoleAdmin, false},
		{RoleMember, RoleOwner, false},

		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleMember, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleOwner, false},

		{RoleOwner, RoleViewer, true},
		{RoleOwner, RoleMember, true},
		{RoleOwner, RoleAdmin, true},
		{RoleOwner, RoleOwner, true},

		// An unset or unrecognised role satisfies nothing: a context that was
		// never populated must fail closed rather than default to viewer.
		{"", RoleViewer, false},
		{"superuser", RoleViewer, false},
		{"OWNER", RoleViewer, false},
		{RoleOwner, "", false},
		{RoleOwner, "root", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.role)+"/at-least/"+string(tt.minimum), func(t *testing.T) {
			t.Parallel()
			if got := tt.role.AtLeast(tt.minimum); got != tt.want {
				t.Fatalf("Role(%q).AtLeast(%q) = %v, want %v", tt.role, tt.minimum, got, tt.want)
			}
		})
	}
}

func TestRoleRoundTripsThroughTheDatabaseEnum(t *testing.T) {
	t.Parallel()

	for _, role := range []Role{RoleViewer, RoleMember, RoleAdmin, RoleOwner} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			if got := RoleFromDB(role.DB()); got != role {
				t.Fatalf("round trip: got %q, want %q", got, role)
			}
		})
	}

	// A value the enum grows later, or a corrupted row, must not become a role.
	if got := RoleFromDB(dbgen.MembershipRole("superuser")); got.Valid() {
		t.Fatalf("RoleFromDB(%q) = %q, want an invalid role", "superuser", got)
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    Role
		wantErr bool
	}{
		{"viewer", RoleViewer, false},
		{"member", RoleMember, false},
		{"admin", RoleAdmin, false},
		{"owner", RoleOwner, false},
		{"", "", true},
		{"Owner", "", true},  // case-sensitive on purpose: the wire form is lowercase
		{" owner", "", true}, // no silent trimming of a wire value
		{"root", "", true},
	}

	for _, tt := range tests {
		t.Run("role="+tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRole(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRole(%q) = %q, want an error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRole(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseRole(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
