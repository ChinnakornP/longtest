package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig tunes the connection pool. The zero value is not usable; use
// DefaultPoolConfig.
type PoolConfig struct {
	// MaxConns caps concurrent database work. Kept well below Postgres'
	// max_connections so that migrations and psql can still get in.
	MaxConns int32
	// MinConns keeps a few warm connections so the first request after an idle
	// period does not pay for a TLS + auth round trip.
	MinConns int32
	// MaxConnLifetime recycles connections so a rolling Postgres restart or a
	// failover does not leave the pool pinned to a dead backend.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime releases connections that nothing is using.
	MaxConnIdleTime time.Duration
	// ConnectTimeout bounds a single dial. Callers still pass a context; this
	// is the ceiling when that context has no deadline of its own.
	ConnectTimeout time.Duration
}

// DefaultPoolConfig returns the settings the backend and the migrator use.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxConns:        16,
		MinConns:        2,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 15 * time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
}

// NewPool opens a connection pool and verifies it can reach the database.
//
// The returned error never contains the DSN verbatim: a Postgres URL carries
// its password inline and these errors are logged.
func NewPool(ctx context.Context, dsn string, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url %s: %w", RedactDSN(dsn), err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool for %s: %w", RedactDSN(dsn), err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s: %w", RedactDSN(dsn), err)
	}
	return pool, nil
}
