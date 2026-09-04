// Package migrations carries the versioned PostgreSQL schema.
//
// Migrations are goose-format (`-- +goose Up` / `-- +goose Down` in a single
// file) and are embedded into the binary, so `migrate` needs nothing but a
// DATABASE_URL to run — no migration directory has to ship alongside a
// container image.
//
// Rules for adding one:
//
//   - Forward-only numbering. Never edit a file that has been applied
//     anywhere; add the next number instead.
//   - Expand -> migrate -> contract. A column is added in one release and
//     dropped in a later one; a rename is add + backfill + drop, never
//     `ALTER ... RENAME` in the same deploy as the code that uses it.
//   - Every table holding customer data gets `org_id uuid NOT NULL
//     REFERENCES organizations (id)` and `UNIQUE (org_id, id)`, and every
//     cross-table reference inside a tenant is a composite foreign key
//     `(org_id, parent_id) -> parent (org_id, id)`. TestSchemaTenancy in
//     internal/db enforces this.
//   - Anything that takes a long lock on a table with real traffic (adding a
//     non-partial index, a NOT NULL without a default, a volatile default)
//     goes into its own migration annotated `-- +goose NO TRANSACTION` and
//     uses CREATE INDEX CONCURRENTLY.
package migrations

import "embed"

// FS holds every migration file, in lexicographic order.
//
//go:embed *.sql
var FS embed.FS
