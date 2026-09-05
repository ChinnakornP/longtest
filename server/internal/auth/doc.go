// Package auth is the identity and tenancy layer of the backend.
//
// It owns four things, and nothing above it may reimplement any of them:
//
//   - Passwords. argon2id, parameters stored with each hash, one comparison
//     path (Hasher). cmd/seed uses the same hasher, so a seeded developer
//     account logs in through exactly the code a real one does.
//   - Sessions. An opaque random token in an httpOnly, SameSite=Lax cookie;
//     only its SHA-256 reaches the database; expiry and revocation are
//     filtered in SQL, so there is no code path that accepts a dead session.
//   - The request-scoped tenancy decision. RequireUser establishes WHO,
//     RequireOrg establishes WHICH ORGANIZATION from the X-Org-ID header plus
//     a membership row, and RequireRole gates the route. The result is an
//     OrgScope in the context, which is the only way a handler can obtain an
//     org id (ADR-006).
//   - Daemon credentials. RequireRuntime / AuthenticateRuntime resolve a
//     runtime token to (orgID, runtimeID) from the token row. Nothing a daemon
//     sends is ever consulted for either value.
//
// Caller, OrgScope and RuntimeCaller are sealed: their fields are unexported,
// the functions that put them into a context are unexported, and no exported
// function builds one out of its arguments. Outside this package the only
// value of those types that can be written down is the zero value, which names
// nobody. That is what makes ADR-006 and ADR-007 properties of the code rather
// than conventions - see seal_test.go, which fails the build if it stops being
// true.
//
// The organization, membership, invite and pairing endpoints live in
// internal/org, which depends on this package. The dependency is one-way: the
// one thing auth needs from org - creating an organization during signup -
// arrives through the OrgCreator interface declared here.
package auth
