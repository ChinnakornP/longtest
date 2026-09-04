// Package db is the backend's data-access layer: the sqlc-generated queries in
// dbgen, plus the transaction helper and the error classification every layer
// above this one relies on.
//
// The rule this package exists to enforce is that a caller can never reach the
// database without naming an organization. Every query in queries/ that touches
// a tenant table takes org_id as a bound parameter, and TestQueriesAreOrgScoped
// fails the build if a new one does not.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
)

// Store owns the connection pool and hands out query sets.
//
// It is safe for concurrent use and is created once at start-up.
type Store struct {
	pool *pgxpool.Pool
	*dbgen.Queries
}

// NewStore wraps an existing pool. The pool's lifetime belongs to the caller.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, Queries: dbgen.New(pool)}
}

// Pool exposes the underlying pool for the few callers that need raw access
// (health checks, LISTEN/NOTIFY). Query code should use the Queries methods.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithTx runs fn inside a single transaction.
//
// Every unit of work that writes more than one row goes through this: the
// alternative is a partially applied change that no later request can detect.
// fn must not retain the *dbgen.Queries it is given — it is bound to a
// connection that is returned to the pool when WithTx returns.
//
// The transaction is rolled back if fn returns an error or panics; a panic is
// re-raised after the rollback so it is not silently converted into a commit.
func (s *Store) WithTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	return s.withTxOptions(ctx, pgx.TxOptions{}, fn)
}

// WithSerializableTx is WithTx at SERIALIZABLE isolation, for the read-compute-
// write sequences that cannot be expressed as a single locking statement. The
// caller is responsible for retrying on a serialization failure (40001).
func (s *Store) WithSerializableTx(ctx context.Context, fn func(q *dbgen.Queries) error) error {
	return s.withTxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
}

func (s *Store) withTxOptions(ctx context.Context, opts pgx.TxOptions, fn func(q *dbgen.Queries) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", Classify(err))
	}

	defer func() {
		if p := recover(); p != nil {
			// Roll back with a context of its own: ctx may already be the
			// reason we are unwinding.
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(dbgen.New(tx)); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", Classify(err))
	}
	return nil
}
