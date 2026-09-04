package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ChinnakornP/longtest/server/migrations"
)

// Migrator applies the embedded schema to a database.
//
// goose speaks database/sql, while the query layer uses pgx natively, so the
// migrator opens its own short-lived *sql.DB over the same pgx driver instead
// of borrowing the application pool. Migrations run rarely and must not hold a
// connection the request path needs.
type Migrator struct {
	db  *sql.DB
	dsn string
}

// NewMigrator opens a dedicated connection for schema work.
//
// Close must be called by the caller.
func NewMigrator(ctx context.Context, dsn string, out io.Writer) (*Migrator, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url %s: %w", RedactDSN(dsn), err)
	}

	sqlDB := stdlib.OpenDB(*cfg)
	// A migration is a single serialised operation; more than one connection
	// only invites a second one to block on goose's advisory lock.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping %s: %w", RedactDSN(dsn), err)
	}

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if out != nil {
		goose.SetLogger(log{out: out})
	}
	if err := goose.SetDialect("postgres"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("select goose dialect: %w", err)
	}

	return &Migrator{db: sqlDB, dsn: dsn}, nil
}

// Close releases the migrator's connection.
func (m *Migrator) Close() error {
	if err := m.db.Close(); err != nil {
		return fmt.Errorf("close migrator connection: %w", err)
	}
	return nil
}

// Up applies every pending migration.
func (m *Migrator) Up(ctx context.Context) error {
	if err := goose.UpContext(ctx, m.db, "."); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back the most recent migration.
func (m *Migrator) Down(ctx context.Context) error {
	if err := goose.DownContext(ctx, m.db, "."); err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// DownAll rolls every migration back, leaving only goose's own version table.
//
// This is what `make migrate-down` runs: the acceptance gate for this schema is
// that up -> down returns the database to an empty state, repeatably.
func (m *Migrator) DownAll(ctx context.Context) error {
	if err := goose.DownToContext(ctx, m.db, ".", 0); err != nil {
		return fmt.Errorf("migrate down to 0: %w", err)
	}
	return nil
}

// Version reports the currently applied migration version.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	v, err := goose.GetDBVersionContext(ctx, m.db)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// Status prints each migration and whether it has been applied.
func (m *Migrator) Status(ctx context.Context) error {
	if err := goose.StatusContext(ctx, m.db, "."); err != nil {
		return fmt.Errorf("migration status: %w", err)
	}
	return nil
}

// log adapts goose's logger onto an io.Writer.
//
// goose's default logger writes to the standard logger and calls os.Exit on
// Fatalf, which would skip our deferred Close.
type log struct{ out io.Writer }

func (l log) Fatalf(format string, v ...any) { l.write(format, v...) }
func (l log) Printf(format string, v ...any) { l.write(format, v...) }

// write terminates every line itself: goose writes for the standard logger,
// which appends the newline for it.
func (l log) write(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	// Migration progress is advisory output; a failed write must not abort a
	// migration that is otherwise succeeding.
	_, _ = fmt.Fprint(l.out, msg)
}
