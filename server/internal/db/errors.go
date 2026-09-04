package db

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Domain-level errors. Everything above this package matches on these, never on
// a pgconn.PgError: a driver error carries the SQL statement and the constraint
// name, and neither belongs in an HTTP response body.
var (
	// ErrNotFound is returned when a query that expects one row finds none.
	// Note that in a tenant-scoped query this also covers "the row exists but
	// belongs to another organization", which is exactly the response a caller
	// should get in that case.
	ErrNotFound = errors.New("not found")

	// ErrConflict is a unique-constraint violation: the row already exists.
	ErrConflict = errors.New("conflict")

	// ErrInvalidReference is a foreign-key violation. In this schema that most
	// often means a cross-organization reference was rejected by a composite
	// foreign key.
	ErrInvalidReference = errors.New("invalid reference")

	// ErrInvalidValue is a CHECK or NOT NULL violation: the value itself is not
	// acceptable to the schema.
	ErrInvalidValue = errors.New("invalid value")

	// ErrSerializationFailure means the transaction lost a write race and the
	// caller may retry it unchanged.
	ErrSerializationFailure = errors.New("serialization failure")
)

// ConstraintError names the constraint that rejected a write, so a service can
// tell "duplicate slug" from "duplicate e-mail" without parsing a driver
// message. The wrapped sentinel is what handlers map to a status code.
type ConstraintError struct {
	// Constraint is the database constraint name, e.g.
	// "runs_org_id_idempotency_key_key". It is safe to log but is not a
	// user-facing string.
	Constraint string
	// Table is the relation the constraint belongs to, when the driver
	// reported one.
	Table string
	kind  error
}

func (e *ConstraintError) Error() string {
	if e.Table != "" {
		return fmt.Sprintf("%s: %s on %s", e.kind, e.Constraint, e.Table)
	}
	return fmt.Sprintf("%s: %s", e.kind, e.Constraint)
}

// Unwrap exposes the sentinel so errors.Is(err, ErrConflict) works.
func (e *ConstraintError) Unwrap() error { return e.kind }

// Classify converts a pgx/pgconn error into one of this package's domain
// errors. It is the single place driver errors are interpreted; callers wrap
// its result rather than inspecting SQLSTATE themselves.
//
// An error it does not recognise is returned unchanged, so an unexpected
// failure surfaces as a 500 rather than being mislabelled.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "23505": // unique_violation
		return &ConstraintError{Constraint: pgErr.ConstraintName, Table: pgErr.TableName, kind: ErrConflict}
	case "23503": // foreign_key_violation
		return &ConstraintError{Constraint: pgErr.ConstraintName, Table: pgErr.TableName, kind: ErrInvalidReference}
	case "23514", "23502": // check_violation, not_null_violation
		return &ConstraintError{Constraint: pgErr.ConstraintName, Table: pgErr.TableName, kind: ErrInvalidValue}
	case "40001": // serialization_failure
		return ErrSerializationFailure
	case "40P01": // deadlock_detected
		return ErrSerializationFailure
	default:
		return err
	}
}

// IsRetryable reports whether a failed transaction can be replayed unchanged.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrSerializationFailure)
}
