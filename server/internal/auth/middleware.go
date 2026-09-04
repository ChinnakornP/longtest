package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// Middleware is where the tenancy rule is enforced, once, for every route.
//
// The chain a tenant-scoped route runs behind is:
//
//	RequireUser  -> a live session cookie          -> Caller in the context
//	RequireOrg   -> X-Org-ID + a membership row    -> OrgScope in the context
//	RequireRole  -> the role gate for this route
//
// A handler that needs an org id calls auth.MustOrgScope(ctx). There is no
// exported way to build an OrgScope from a request, so "read the org id out of
// the body" is not a mistake a handler can make - the type it needs simply
// cannot be obtained that way.

// RequireUser rejects a request without a live session.
//
// It is the only place the session cookie is read. Everything downstream sees
// a Caller in the context or never runs.
func RequireUser(sessions *Sessions) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := sessions.TokenFromRequest(r)
			if err != nil {
				httpx.WriteError(w, r, httpx.Unauthorized("sign in to continue"))
				return
			}

			caller, err := sessions.Authenticate(r.Context(), token)
			if err != nil {
				if errors.Is(err, ErrInvalidCredentials) {
					// The cookie is dead: clear it so the browser stops
					// sending it on every subsequent request.
					sessions.ClearCookie(w)
					httpx.WriteError(w, r, httpx.Unauthorized("your session has expired, sign in again"))
					return
				}
				httpx.WriteError(w, r, fmt.Errorf("authenticate session: %w", err))
				return
			}

			ctx := WithCaller(r.Context(), caller)
			ctx = httpx.WithLogger(ctx, httpx.LoggerFrom(ctx).With(slog.String("user_id", caller.UserID.String())))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireOrg resolves the active organization from the X-Org-ID header and
// verifies the caller's membership in it.
//
// Everything that goes wrong here is a 403, deliberately and per the task
// contract: a missing header, an unparsable id, an organization that does not
// exist and an organization the caller is not a member of are indistinguishable
// to the client. Any finer-grained answer would tell an outsider which
// organization ids are real.
func RequireOrg(store Store) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, ok := CallerFrom(r.Context())
			if !ok {
				// The route was wired without RequireUser. Fail closed.
				httpx.WriteError(w, r, httpx.Unauthorized("sign in to continue"))
				return
			}

			raw := strings.TrimSpace(r.Header.Get(OrgHeader))
			if raw == "" {
				httpx.WriteError(w, r, httpx.Forbidden("%s is required", OrgHeader))
				return
			}
			orgID, err := uuid.Parse(raw)
			if err != nil {
				httpx.WriteError(w, r, httpx.Forbidden("%s is not a valid organization id", OrgHeader))
				return
			}

			membership, err := store.GetMembership(r.Context(), dbgen.GetMembershipParams{OrgID: orgID, UserID: caller.UserID})
			if err != nil {
				if errors.Is(db.Classify(err), db.ErrNotFound) {
					httpx.WriteError(w, r, httpx.Forbidden("you are not a member of that organization"))
					return
				}
				httpx.WriteError(w, r, fmt.Errorf("look up membership: %w", db.Classify(err)))
				return
			}

			role := RoleFromDB(membership.Role)
			if !role.Valid() {
				// An unrecognised enum value must not be treated as a role.
				httpx.WriteError(w, r, httpx.Forbidden("your membership is not usable"))
				return
			}

			scope := OrgScope{Caller: caller, OrgID: orgID, Role: role}
			ctx := WithOrgScope(r.Context(), scope)
			ctx = httpx.WithLogger(ctx, httpx.LoggerFrom(ctx).With(
				slog.String("org_id", orgID.String()),
				slog.String("role", role.String()),
			))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole gates a route on a minimum role.
//
//	RequireRole(RoleViewer)  every member, including read-only ones
//	RequireRole(RoleMember)  create projects, start runs
//	RequireRole(RoleAdmin)   manage members, invites and runtimes
func RequireRole(minimum Role) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope, err := MustOrgScope(r.Context())
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if !scope.Role.AtLeast(minimum) {
				httpx.WriteError(w, r, httpx.Forbidden(
					"this action needs the %s role or higher; you are %s", minimum, scope.Role))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireOrgMatchesPath asserts that an {orgID} path segment names the same
// organization as the X-Org-ID header.
//
// ADR-006 rejected the path as a SOURCE of the org id, and it still is not
// one: RequireOrg has already resolved the scope from the header before this
// runs. The path segment is a redundant assertion, so a client that switches
// organizations without updating a URL gets a 403 instead of quietly operating
// on the wrong tenant.
func RequireOrgMatchesPath(param string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope, err := MustOrgScope(r.Context())
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			pathOrg, err := httpx.PathUUID(r, param)
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if pathOrg != scope.OrgID {
				httpx.WriteError(w, r, httpx.Forbidden(
					"the organization in the path does not match %s", OrgHeader))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRuntime authenticates a daemon by its runtime token.
//
// The org and runtime ids come from the token row and are put into the
// context; nothing the daemon sends is consulted. This is what T09's WebSocket
// upgrade runs behind, and it is why a `hello` frame claiming another org's
// runtime id cannot do anything.
func RequireRuntime(store Store) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := BearerToken(r)
			if err != nil {
				httpx.WriteError(w, r, httpx.Unauthorized("a runtime token is required"))
				return
			}

			caller, err := AuthenticateRuntime(r.Context(), store, token)
			if err != nil {
				if errors.Is(err, ErrInvalidCredentials) {
					httpx.WriteError(w, r, httpx.Unauthorized("that runtime token is not valid"))
					return
				}
				httpx.WriteError(w, r, err)
				return
			}

			ctx := WithRuntimeCaller(r.Context(), caller)
			ctx = httpx.WithLogger(ctx, httpx.LoggerFrom(ctx).With(
				slog.String("org_id", caller.OrgID.String()),
				slog.String("runtime_id", caller.RuntimeID.String()),
			))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// BearerToken extracts the credential from an Authorization header.
func BearerToken(r *http.Request) (string, error) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", ErrInvalidCredentials
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", ErrInvalidCredentials
	}
	return token, nil
}

// AuthenticateRuntime resolves a runtime token to the daemon behind it.
//
// It is exported because the WebSocket upgrade in T09 authenticates before it
// has an http.Handler chain to run: the check has to be identical, so it is
// the same function rather than a second implementation.
//
// A revoked token, a token for a deleted runtime and a disabled runtime are
// all ErrInvalidCredentials: a daemon that has been turned off must not be
// able to tell why.
func AuthenticateRuntime(ctx context.Context, store Store, token string) (RuntimeCaller, error) {
	hash, err := HashBearerToken(token)
	if err != nil {
		return RuntimeCaller{}, ErrInvalidCredentials
	}

	row, err := store.GetLiveRuntimeTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return RuntimeCaller{}, ErrInvalidCredentials
		}
		return RuntimeCaller{}, fmt.Errorf("look up runtime token: %w", db.Classify(err))
	}

	// The token is live, but the runtime it belongs to may have been disabled.
	// This read is org-scoped by the token row, never by the caller.
	runtime, err := store.GetRuntime(ctx, dbgen.GetRuntimeParams{OrgID: row.OrgID, ID: row.RuntimeID})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return RuntimeCaller{}, ErrInvalidCredentials
		}
		return RuntimeCaller{}, fmt.Errorf("look up runtime: %w", db.Classify(err))
	}
	if runtime.DisabledAt.Valid {
		return RuntimeCaller{}, ErrInvalidCredentials
	}

	// Advisory: powers "last seen" and must not fail the request.
	if _, err := store.TouchRuntimeToken(ctx, row.ID); err != nil {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not record runtime token use", "err", db.Classify(err))
	}

	return RuntimeCaller{OrgID: row.OrgID, RuntimeID: row.RuntimeID, TokenID: row.ID}, nil
}
