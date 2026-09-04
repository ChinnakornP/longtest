package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

// config is the process configuration, read once at start-up.
//
// Everything comes from the environment - there is no config file and no
// default DSN - so a deployment cannot accidentally inherit a developer's
// database, and nothing secret is ever committed.
type config struct {
	Addr           string
	DatabaseURL    string
	LogLevel       slog.Level
	CORSOrigins    []string
	SessionCookie  auth.SessionConfig
	RequestTimeout time.Duration
	ShutdownGrace  time.Duration
}

func loadConfig() (config, error) {
	dsn, err := pgdb.DSNFromEnv()
	if err != nil {
		return config{}, fmt.Errorf("%w (copy .env.example to .env)", err)
	}

	session := auth.DefaultSessionConfig()
	// Secure defaults to on. Local development over plain http has to opt out,
	// because a browser silently drops a Secure cookie on an http:// origin and
	// the resulting "login does nothing" is hard to diagnose.
	secure, err := boolEnv("SESSION_COOKIE_SECURE", true)
	if err != nil {
		return config{}, err
	}
	session.Secure = secure
	session.Domain = os.Getenv("SESSION_COOKIE_DOMAIN")
	ttl, err := durationEnv("SESSION_TTL", session.TTL)
	if err != nil {
		return config{}, err
	}
	session.TTL = ttl

	timeout, err := durationEnv("SERVER_REQUEST_TIMEOUT", 30*time.Second)
	if err != nil {
		return config{}, err
	}

	return config{
		Addr:           stringEnv("SERVER_HTTP_ADDR", "127.0.0.1:8080"),
		DatabaseURL:    dsn,
		LogLevel:       logLevel(os.Getenv("SERVER_LOG_LEVEL")),
		CORSOrigins:    splitOrigins(os.Getenv("CORS_ALLOWED_ORIGINS")),
		SessionCookie:  session,
		RequestTimeout: timeout,
		ShutdownGrace:  10 * time.Second,
	}, nil
}

func stringEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func boolEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return v, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 30s, got %q", key, raw)
	}
	return v, nil
}

// splitOrigins parses the comma-separated CORS allowlist. An empty value means
// no cross-origin browser access at all, which is the right default for a
// deployment that serves the web app from the same origin.
func splitOrigins(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func logLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
