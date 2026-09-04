// Package db holds the shared database plumbing for the backend: connection
// configuration and, from T02 onwards, the sqlc-generated query layer.
package db

import (
	"fmt"
	"net/url"
	"os"
)

// ErrNoDSN is returned when the database configuration is missing.
var ErrNoDSN = fmt.Errorf("DATABASE_URL is not set")

// DSNFromEnv returns the configured database DSN.
func DSNFromEnv() (string, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return "", ErrNoDSN
	}
	return dsn, nil
}

// RedactDSN replaces the password in a database URL with a fixed placeholder.
//
// Every log line, error message and support bundle that mentions a DSN has to
// go through this first: a Postgres URL carries its password inline, and the
// backend's logs are shipped off the machine.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		// An unparsable value may still be a DSN with a password in it, so it
		// is never echoed back verbatim.
		if err != nil {
			return "[unparsable dsn]"
		}
		return dsn
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return dsn
	}
	u.User = url.UserPassword(u.User.Username(), "xxxxx")
	return u.String()
}
