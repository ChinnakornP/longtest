package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/server/internal/artifact"
	"github.com/ChinnakornP/longtest/server/internal/auth"
	runpkg "github.com/ChinnakornP/longtest/server/internal/run"
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

	// Run and Scheduler configure the queue. Both have working defaults; a
	// deployment only touches them to widen the liveness window on a slow
	// network or to allow a retry after a lost runtime.
	Run       runpkg.Config
	Scheduler runpkg.SchedulerConfig
	// Artifacts is the object-storage presigner, or a disabled one when S3 is
	// not configured. It is built at start-up so a bad endpoint fails the
	// process rather than the first runpkg.
	Artifacts *artifact.Service
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

	addr := stringEnv("SERVER_HTTP_ADDR", "127.0.0.1:8080")

	runCfg := runpkg.DefaultConfig()
	if runCfg.OnlineWithin, err = durationEnv("RUNTIME_ONLINE_WITHIN", runCfg.OnlineWithin); err != nil {
		return config{}, err
	}
	if runCfg.MaxAttempts, err = int32Env("RUN_MAX_ATTEMPTS", runCfg.MaxAttempts); err != nil {
		return config{}, err
	}
	if runCfg.PresignBaseURL, err = publicURL(addr); err != nil {
		return config{}, err
	}

	artifacts, err := artifactService()
	if err != nil {
		return config{}, err
	}

	return config{
		Addr:           addr,
		DatabaseURL:    dsn,
		LogLevel:       logLevel(os.Getenv("SERVER_LOG_LEVEL")),
		CORSOrigins:    splitOrigins(os.Getenv("CORS_ALLOWED_ORIGINS")),
		SessionCookie:  session,
		RequestTimeout: timeout,
		ShutdownGrace:  10 * time.Second,
		Run:            runCfg,
		Scheduler:      runpkg.DefaultSchedulerConfig(),
		Artifacts:      artifacts,
	}, nil
}

// publicURL is the origin a DAEMON reaches this API on. It ends up inside a
// runpkg.assign frame as the base of the artifact upload endpoint, so a value
// that only resolves inside this process's own network makes uploads fail on
// the customer's machine — hence the explicit variable, with the listen
// address as a local-development fallback.
func publicURL(addr string) (string, error) {
	raw := strings.TrimSpace(os.Getenv("SERVER_PUBLIC_URL"))
	if raw == "" {
		return "http://" + addr, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("SERVER_PUBLIC_URL must be an absolute http(s) url, got %q", raw)
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// artifactService builds the presigner from the S3 variables.
//
// With none of them set the service is disabled and the API still runs: a
// deployment without object storage can create projects, start runs and read
// reports, and only artifact upload and download return 503. With SOME of them
// set it is a start-up error, because a half-configured bucket is a
// misconfiguration rather than a choice.
func artifactService() (*artifact.Service, error) {
	cfg := artifact.Config{
		Endpoint:       strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
		PublicEndpoint: strings.TrimSpace(os.Getenv("S3_PUBLIC_ENDPOINT")),
		Region:         stringEnv("S3_REGION", "us-east-1"),
		Bucket:         strings.TrimSpace(os.Getenv("S3_BUCKET")),
		Credentials: artifact.Credentials{
			AccessKeyID:     strings.TrimSpace(os.Getenv("S3_ACCESS_KEY_ID")),
			SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		},
	}
	// Path style by default: MinIO and any endpoint that is a bare IP require
	// it, and those are what local development and a customer's own object
	// store look like.
	pathStyle, err := boolEnv("S3_PATH_STYLE", true)
	if err != nil {
		return nil, err
	}
	cfg.PathStyle = pathStyle

	if cfg.PresignTTL, err = durationEnv("S3_PRESIGN_TTL", 15*time.Minute); err != nil {
		return nil, err
	}
	if cfg.UploadWindow, err = durationEnv("S3_UPLOAD_WINDOW", artifact.MaxUploadWindow); err != nil {
		return nil, err
	}

	if cfg.Endpoint == "" && cfg.Bucket == "" &&
		cfg.Credentials.AccessKeyID == "" && cfg.Credentials.SecretAccessKey == "" {
		return artifact.Disabled(), nil
	}
	service, err := artifact.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("object storage is partly configured: %w", err)
	}
	return service, nil
}

func int32Env(key string, fallback int32) (int32, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return int32(value), nil
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
