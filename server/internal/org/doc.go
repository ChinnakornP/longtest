// Package org owns organizations, memberships, invites and runtime pairing -
// the multi-tenant boundary every other service scopes its queries by.
//
// It sits directly on top of internal/auth: the caller's identity, active
// organization and role are already established by the middleware before any
// handler here runs, so the rules this package adds are the ones that need
// business context rather than a request:
//
//   - a user who creates an organization becomes its owner, in one transaction
//     with the organization row;
//   - nobody may invite somebody to a role above their own, so an admin cannot
//     mint an owner;
//   - an invite is accepted only by the address it was issued to, exactly
//     once, and never downgrades an existing membership;
//   - a pairing code is single-use and expires in 15 minutes, and the runtime
//     token it yields is the daemon's only proof of which organization it
//     belongs to.
package org
