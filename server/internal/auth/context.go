package auth

import (
	"context"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

type contextKey int

const (
	callerKey contextKey = iota
	orgKey
	runtimeKey
)

// The three types below are the request's answers to "who", "which tenant"
// and "which daemon". Every one of their fields is unexported and read through
// an accessor, and the functions that build them are unexported too.
//
// That is not style. It is the mechanism behind ADR-006 and ADR-007: outside
// this package the only value of any of these types that can be written down
// is the zero value, which names no user, no organization and no runtime and
// is refused by MustOrgScope and MustRuntimeCaller. A handler cannot assemble
// a scope out of a path segment, a body or a query string, because the struct
// literal that would do it does not compile.
//
// TestAuthPrincipalsAreSealed in seal_test.go holds this shut.

// Caller is the signed-in human behind a request. It is put into the context
// by RequireUser and is all a handler ever needs to know about "who".
//
// It is produced only by Sessions.Authenticate, from a live session row.
type Caller struct {
	userID    uuid.UUID
	sessionID uuid.UUID
	email     string
	name      string
}

// UserID is the signed-in user. It is uuid.Nil on a zero Caller, which is the
// only Caller a package outside auth can construct.
func (c Caller) UserID() uuid.UUID { return c.userID }

// SessionID is the session the request authenticated with.
func (c Caller) SessionID() uuid.UUID { return c.sessionID }

// Email is the caller's address as stored, not as typed.
func (c Caller) Email() string { return c.email }

// Name is the caller's display name.
func (c Caller) Name() string { return c.name }

// OrgScope is the tenancy scope of a request: who is calling, which
// organization they selected with X-Org-ID, and what they may do in it.
//
// It is produced only by RequireOrg, which verifies the membership itself.
// There is no exported constructor and no settable field, so a handler cannot
// invent a scope for an organization the caller does not belong to: the org id
// it queries with can only be one auth resolved from the header.
type OrgScope struct {
	caller Caller
	orgID  uuid.UUID
	role   Role
}

// OrgID is the active organization. Every org-scoped query binds this value.
func (s OrgScope) OrgID() uuid.UUID { return s.orgID }

// Role is the caller's role in the active organization.
func (s OrgScope) Role() Role { return s.role }

// Caller is the human this scope was resolved for.
func (s OrgScope) Caller() Caller { return s.caller }

// UserID forwards to the caller, so a service that only wants "who did this"
// does not have to reach through Caller.
func (s OrgScope) UserID() uuid.UUID { return s.caller.userID }

// Email forwards to the caller.
func (s OrgScope) Email() string { return s.caller.email }

// Name forwards to the caller.
func (s OrgScope) Name() string { return s.caller.name }

// RuntimeCaller is a daemon authenticated by its runtime token.
//
// OrgID and RuntimeID come from the token row, never from anything the daemon
// sent. That is the whole point of this type: a handler that reads these
// values cannot accidentally read a runtime id out of a frame instead, and no
// package outside auth can write one down.
type RuntimeCaller struct {
	orgID     uuid.UUID
	runtimeID uuid.UUID
	tokenID   uuid.UUID
}

// OrgID is the organization the token belongs to.
func (rc RuntimeCaller) OrgID() uuid.UUID { return rc.orgID }

// RuntimeID is the runtime the token belongs to.
func (rc RuntimeCaller) RuntimeID() uuid.UUID { return rc.runtimeID }

// TokenID is the runtime_tokens row the daemon presented.
func (rc RuntimeCaller) TokenID() uuid.UUID { return rc.tokenID }

// withCaller is unexported on purpose: RequireUser is the only thing that may
// declare who a request is from.
func withCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFrom returns the signed-in user, if the request went through
// RequireUser.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey).(Caller)
	return c, ok
}

// withOrgScope is unexported on purpose: RequireOrg, which has checked the
// membership row, is the only thing that may put a scope into a context.
func withOrgScope(ctx context.Context, s OrgScope) context.Context {
	return context.WithValue(ctx, orgKey, s)
}

// OrgScopeFrom returns the tenancy scope, if the request went through
// RequireOrg.
func OrgScopeFrom(ctx context.Context) (OrgScope, bool) {
	s, ok := ctx.Value(orgKey).(OrgScope)
	return s, ok
}

// MustOrgScope is what handlers call. A missing scope means the route was
// registered without RequireOrg, so it fails closed with a 403 rather than
// falling back to an unscoped query.
func MustOrgScope(ctx context.Context) (OrgScope, error) {
	scope, ok := OrgScopeFrom(ctx)
	if !ok || scope.orgID == uuid.Nil {
		return OrgScope{}, httpx.Forbidden("%s is required", OrgHeader)
	}
	return scope, nil
}

// withRuntimeCaller is unexported on purpose: RequireRuntime and the WebSocket
// upgrade reach it through AuthenticateRuntime, and nothing else may.
func withRuntimeCaller(ctx context.Context, rc RuntimeCaller) context.Context {
	return context.WithValue(ctx, runtimeKey, rc)
}

// RuntimeCallerFrom returns the authenticated daemon, if the request went
// through RequireRuntime.
func RuntimeCallerFrom(ctx context.Context) (RuntimeCaller, bool) {
	rc, ok := ctx.Value(runtimeKey).(RuntimeCaller)
	return rc, ok
}

// MustRuntimeCaller is the daemon-side counterpart of MustOrgScope.
func MustRuntimeCaller(ctx context.Context) (RuntimeCaller, error) {
	rc, ok := RuntimeCallerFrom(ctx)
	if !ok || rc.orgID == uuid.Nil {
		return RuntimeCaller{}, httpx.Unauthorized("a runtime token is required")
	}
	return rc, nil
}
