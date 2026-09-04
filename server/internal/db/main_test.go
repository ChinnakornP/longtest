package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	// pkg/db is also package `db`; aliased so both can be used here.
	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

// The tests in this package that need a database create their own throwaway one
// from TEST_DATABASE_URL (falling back to DATABASE_URL) and drop it afterwards,
// so they never touch a developer's working data. When neither is set they skip,
// which keeps `go test ./...` green on a machine with no Postgres - CI runs them
// with a service container via `make test-db`.
const (
	adminDSNEnv    = "TEST_DATABASE_URL"
	fallbackDSNEnv = "DATABASE_URL"
)

var testStore *Store

func TestMain(m *testing.M) {
	dsn := os.Getenv(adminDSNEnv)
	if dsn == "" {
		dsn = os.Getenv(fallbackDSNEnv)
	}
	if dsn == "" {
		// No database: the DB-backed tests skip, the static ones still run.
		os.Exit(m.Run())
	}

	code, err := runWithDatabase(dsn, m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testdb:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runWithDatabase provisions a scratch database, migrates it, runs the suite and
// drops the database again. It returns the suite's exit code.
func runWithDatabase(adminDSN string, m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	name, err := scratchDatabaseName()
	if err != nil {
		return 0, err
	}

	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, fmt.Errorf("connect to %s: %w", pgdb.RedactDSN(adminDSN), err)
	}
	// The identifier is generated here from hex, never from user input, and is
	// quoted anyway; CREATE DATABASE cannot take a bind parameter.
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		_ = admin.Close(ctx)
		return 0, fmt.Errorf("create scratch database: %w", err)
	}
	_ = admin.Close(ctx)

	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		conn, cerr := pgx.Connect(dropCtx, adminDSN)
		if cerr != nil {
			fmt.Fprintln(os.Stderr, "testdb: cannot drop scratch database:", cerr)
			return
		}
		defer func() { _ = conn.Close(dropCtx) }()
		if _, derr := conn.Exec(dropCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); derr != nil {
			fmt.Fprintln(os.Stderr, "testdb: cannot drop scratch database:", derr)
		}
	}()

	testDSN, err := withDatabase(adminDSN, name)
	if err != nil {
		return 0, err
	}

	migrator, err := pgdb.NewMigrator(ctx, testDSN, nil)
	if err != nil {
		return 0, err
	}
	if err := migrator.Up(ctx); err != nil {
		_ = migrator.Close()
		return 0, err
	}
	if err := migrator.Close(); err != nil {
		return 0, err
	}

	pool, err := pgdb.NewPool(ctx, testDSN, pgdb.DefaultPoolConfig())
	if err != nil {
		return 0, err
	}
	testStore = NewStore(pool)
	code := m.Run()
	pool.Close()
	return code, nil
}

func scratchDatabaseName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate scratch database name: %w", err)
	}
	return "qa_test_" + hex.EncodeToString(b[:]), nil
}

// withDatabase rewrites a DSN to point at a different database on the same server.
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", pgdb.RedactDSN(dsn), err)
	}
	u.Path = "/" + name
	return u.String(), nil
}

// requireDB skips a test when no database was provisioned.
func requireDB(t *testing.T) *Store {
	t.Helper()
	if testStore == nil {
		t.Skipf("set %s (or %s) to run the database-backed tests", adminDSNEnv, fallbackDSNEnv)
	}
	return testStore
}

// --- fixtures -------------------------------------------------------------
//
// Every test builds its own organization, so tests share one database without
// sharing any rows.

func newOrg(t *testing.T, s *Store) dbgen.Organization {
	t.Helper()
	slug := "org-" + uuid.NewString()
	org, err := s.CreateOrganization(t.Context(), dbgen.CreateOrganizationParams{
		Name: "Test " + slug,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	return org
}

func newProject(t *testing.T, s *Store, orgID uuid.UUID) dbgen.Project {
	t.Helper()
	p, err := s.CreateProject(t.Context(), dbgen.CreateProjectParams{
		OrgID:   orgID,
		Name:    "project-" + uuid.NewString(),
		BaseURL: "https://demo.example.com",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func newRuntime(t *testing.T, s *Store, orgID uuid.UUID) dbgen.Runtime {
	t.Helper()
	r, err := s.CreateRuntime(t.Context(), dbgen.CreateRuntimeParams{
		OrgID: orgID,
		Name:  "runtime-" + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	return r
}

func newRun(t *testing.T, s *Store, orgID, projectID uuid.UUID, runtimeID uuid.NullUUID) dbgen.Run {
	t.Helper()
	r, err := s.CreateRun(t.Context(), dbgen.CreateRunParams{
		OrgID:     orgID,
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Mode:      dbgen.RunModeFull,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r
}
