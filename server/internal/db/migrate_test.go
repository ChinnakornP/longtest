package db

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

// TestMigrationsRoundTrip is the acceptance test for the schema's reversibility:
// up then down must leave the database empty, and doing it repeatedly must keep
// working. A down migration that forgets a type, function or trigger passes a
// single round trip and fails the second one, which is why this loops.
func TestMigrationsRoundTrip(t *testing.T) {
	adminDSN := os.Getenv(adminDSNEnv)
	if adminDSN == "" {
		adminDSN = os.Getenv(fallbackDSNEnv)
	}
	if adminDSN == "" {
		t.Skipf("set %s (or %s) to run the migration round-trip test", adminDSNEnv, fallbackDSNEnv)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	dsn := provisionScratchDatabase(ctx, t, adminDSN)

	migrator, err := pgdb.NewMigrator(ctx, dsn, nil)
	if err != nil {
		t.Fatalf("open migrator: %v", err)
	}
	t.Cleanup(func() {
		if err := migrator.Close(); err != nil {
			t.Errorf("close migrator: %v", err)
		}
	})

	for round := 1; round <= 3; round++ {
		if err := migrator.Up(ctx); err != nil {
			t.Fatalf("round %d: up: %v", round, err)
		}

		version, err := migrator.Version(ctx)
		if err != nil {
			t.Fatalf("round %d: version: %v", round, err)
		}
		if version == 0 {
			t.Fatalf("round %d: schema version is still 0 after up", round)
		}

		// Sanity check that "up" really built the schema, so an empty database
		// after "down" is not just an empty database throughout.
		if objects := publicObjects(ctx, t, dsn); len(objects) == 0 {
			t.Fatalf("round %d: up created no schema objects", round)
		}

		if err := migrator.DownAll(ctx); err != nil {
			t.Fatalf("round %d: down-all: %v", round, err)
		}

		leftovers := publicObjects(ctx, t, dsn)
		if len(leftovers) > 0 {
			t.Fatalf("round %d: down-all left %d object(s) behind: %s",
				round, len(leftovers), strings.Join(leftovers, ", "))
		}
	}
}

// provisionScratchDatabase creates a database for one test and drops it after.
func provisionScratchDatabase(ctx context.Context, t *testing.T, adminDSN string) string {
	t.Helper()

	name, err := scratchDatabaseName()
	if err != nil {
		t.Fatalf("name scratch database: %v", err)
	}

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to %s: %v", pgdb.RedactDSN(adminDSN), err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create scratch database: %v", err)
	}
	_ = admin.Close(ctx)

	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(dropCtx, adminDSN)
		if err != nil {
			t.Logf("cannot drop scratch database %s: %v", name, err)
			return
		}
		defer func() { _ = conn.Close(dropCtx) }()
		if _, err := conn.Exec(dropCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Logf("cannot drop scratch database %s: %v", name, err)
		}
	})

	dsn, err := withDatabase(adminDSN, name)
	if err != nil {
		t.Fatalf("build scratch dsn: %v", err)
	}
	return dsn
}

// publicObjects lists everything the migrations create in the public schema,
// excluding goose's own bookkeeping table. An empty result means "down" was
// complete: tables, enum types, functions and the citext extension are all
// counted, because forgetting any one of them breaks the next "up".
func publicObjects(ctx context.Context, t *testing.T, dsn string) []string {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	rows, err := conn.Query(ctx, `
		SELECT 'table:' || tablename
		FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
		UNION ALL
		SELECT 'type:' || t.typname
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public' AND t.typtype = 'e'
		UNION ALL
		SELECT 'function:' || p.proname
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'
		UNION ALL
		SELECT 'extension:' || extname
		FROM pg_extension
		WHERE extname <> 'plpgsql'`)
	if err != nil {
		t.Fatalf("list public objects: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan object name: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate public objects: %v", err)
	}
	sort.Strings(out)
	return out
}
