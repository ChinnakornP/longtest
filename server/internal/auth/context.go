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

// Caller is the signed-in human behind a request. It is put into the context
// by RequireUser and is all a handler ever needs to know about "who".
type Caller struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Email     string
	Name      string
}

// OrgScope is the tenancy scope of a request: who is calling, which
// organization they selected with X-Org-ID, and what they may do in it.
//
// It is produced only by RequireOrg, which verifies the membership itself.
// There is no constructor a handler can reach, so a handler cannot invent a
// scope for an organization the caller does not belong to.
type OrgScope struct {
	Caller
	OrgID uuid.UUID
	Role  Role
}

// RuntimeCaller is a daemon authenticated by its runtime token.
//
// OrgID and RuntimeID come from the token row, never from anything the daemon
// sent. That is the whole point of this type: a handler that reads these
// fields cannot accidentally read a runtime id out of a frame instead.
type RuntimeCaller struct {
	OrgID     uuid.UUID
	RuntimeID uuid.UUID
	TokenID   uuid.UUID
}

// WithCaller is exported for tests and for the middleware in this package.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey, c)
}

// CallerFrom returns the signed-in user, if the request went through
// RequireUser.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey).(Caller)
	return c, ok
}

// WithOrgScope is exported for tests and for the middleware in this package.
func WithOrgScope(ctx context.Context, s OrgScope) context.Context {
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
	if !ok || scope.OrgID == uuid.Nil {
		return OrgScope{}, httpx.Forbidden("%s is required", OrgHeader)
	}
	return scope, nil
}

// WithRuntimeCaller is exported for tests and for the middleware in this
// package.
func WithRuntimeCaller(ctx context.Context, rc RuntimeCaller) context.Context {
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
	if !ok || rc.OrgID == uuid.Nil {
		return RuntimeCaller{}, httpx.Unauthorized("a runtime token is required")
	}
	return rc, nil
}
