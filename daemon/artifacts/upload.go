package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ChinnakornP/longtest/daemon/pkg/qaschema"
)

// prefixPattern is the same expression the daemon-envelope contract puts on
// ArtifactUpload.keyPrefix. It is re-checked here rather than assumed: the
// prefix is the tenant boundary, and a prefix whose run segment could be ".."
// bounds nothing.
var prefixPattern = regexp.MustCompile(
	`^orgs/[A-Za-z0-9][A-Za-z0-9._-]{0,199}/runs/[0-9]{4}-[0-9]{2}-[0-9]{2}/[A-Za-z0-9][A-Za-z0-9._-]{0,199}/$`)

// segmentPattern is one path segment below the prefix: a test case ref or a
// file name, both of which can be derived from what a model wrote.
var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

var (
	// ErrPrefixExpired means the presigned credentials in a run.assign are no
	// longer usable. ADR-002 classes this as an environment error: the fix is
	// a fresh URL from the backend, not a retry of the same PUT.
	ErrPrefixExpired = errors.New("artifacts: presigned upload window expired")
	// ErrOutsidePrefix means a key would be written outside the run's own
	// storage prefix.
	ErrOutsidePrefix = errors.New("artifacts: key is outside the run prefix")
)

// ValidatePrefix checks a keyPrefix from run.assign before anything is written
// under it.
func ValidatePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("artifacts: keyPrefix %q does not match the contract pattern", prefix)
	}
	return nil
}

// KeyUnder builds an object key below a validated prefix:
//
//	orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/{testCaseRef}/{filename}
//
// The test case segment is omitted for run-level evidence such as a discovery
// HAR, which is why segments is variadic.
func KeyUnder(prefix string, segments ...string) (string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return "", errors.New("artifacts: no key segments")
	}
	for _, segment := range segments {
		if !segmentPattern.MatchString(segment) {
			return "", fmt.Errorf("artifacts: %q is not a usable key segment", segment)
		}
	}
	return prefix + strings.Join(segments, "/"), nil
}

// KeyWithinPrefix rejects a key the executor reported that does not live under
// this run's prefix. The executor builds its keys from the prefix the daemon
// handed it, so a mismatch is either a bug or a document that did not come
// from this run — both are refusals, not warnings.
func KeyWithinPrefix(prefix, key string) error {
	if err := ValidatePrefix(prefix); err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" {
		return fmt.Errorf("%w: %q", ErrOutsidePrefix, key)
	}
	for _, segment := range strings.Split(rest, "/") {
		if !segmentPattern.MatchString(segment) {
			return fmt.Errorf("%w: %q has an unusable segment %q", ErrOutsidePrefix, key, segment)
		}
	}
	return nil
}

// Upload is one local file to put in object storage.
type Upload struct {
	// Key is the full object key, already below the run prefix.
	Key string
	// Path is the local file the executor or an agent wrote.
	Path string
	// Kind is what the evidence is, as the execution-result contract names it.
	Kind qaschema.ArtifactKind
	// ID is the run-local handle steps and findings point at. Generated when
	// empty.
	ID string
	// ContentType overrides the type guessed from the file extension.
	ContentType string
}

// Uploader puts run evidence straight into object storage with the presigned
// credentials that came down with run.assign. Bytes never travel through the
// API (ADR-002).
type Uploader struct {
	client    *http.Client
	base      *url.URL
	prefix    string
	expiresAt time.Time
	attempts  int
	backoff   time.Duration
	logger    *slog.Logger
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
}

// UploaderOption customises an Uploader.
type UploaderOption func(*Uploader)

// WithHTTPClient replaces the HTTP client, e.g. to point at a test server or
// to configure a corporate egress proxy.
func WithHTTPClient(c *http.Client) UploaderOption {
	return func(u *Uploader) { u.client = c }
}

// WithLogger attaches a logger. Nothing it writes contains a presigned URL:
// the query string of one is a bearer credential.
func WithLogger(l *slog.Logger) UploaderOption {
	return func(u *Uploader) { u.logger = l }
}

// WithRetry sets how many attempts each object gets and the base backoff.
func WithRetry(attempts int, backoff time.Duration) UploaderOption {
	return func(u *Uploader) {
		u.attempts = attempts
		u.backoff = backoff
	}
}

// WithClock replaces the clock and the sleeper, so expiry and backoff are
// testable without waiting.
func WithClock(now func() time.Time, sleep func(context.Context, time.Duration) error) UploaderOption {
	return func(u *Uploader) {
		u.now = now
		u.sleep = sleep
	}
}

// NewUploader validates the presigned credentials from run.assign and returns
// an uploader bound to that run's prefix.
func NewUploader(spec qaschema.ArtifactUpload, opts ...UploaderOption) (*Uploader, error) {
	if err := ValidatePrefix(spec.KeyPrefix); err != nil {
		return nil, err
	}
	base, err := url.Parse(spec.PresignedPutBase)
	if err != nil {
		return nil, fmt.Errorf("artifacts: parse presigned base: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("artifacts: presigned base scheme %q is not http(s)", base.Scheme)
	}
	expiresAt, err := time.Parse(time.RFC3339, spec.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("artifacts: parse expiresAt: %w", err)
	}

	u := &Uploader{
		client:    &http.Client{Timeout: 10 * time.Minute},
		base:      base,
		prefix:    spec.KeyPrefix,
		expiresAt: expiresAt,
		attempts:  3,
		backoff:   500 * time.Millisecond,
		logger:    slog.New(slog.DiscardHandler),
		now:       time.Now,
		sleep:     sleepCtx,
	}
	for _, opt := range opts {
		opt(u)
	}
	return u, nil
}

// Prefix is the run prefix every key must live under.
func (u *Uploader) Prefix() string { return u.prefix }

// ExpiresAt is when the presigned credentials stop working.
func (u *Uploader) ExpiresAt() time.Time { return u.expiresAt }

// UploadAll uploads every file and returns the artifact records to put in
// run.result. It stops at the first failure: a partial evidence set reported
// as complete is worse than a failed run, because the report would then be
// missing exactly the artifact somebody is looking for.
func (u *Uploader) UploadAll(ctx context.Context, uploads []Upload) ([]qaschema.Artifact, error) {
	out := make([]qaschema.Artifact, 0, len(uploads))
	for _, upload := range uploads {
		artifact, err := u.Upload(ctx, upload)
		if err != nil {
			return out, err
		}
		out = append(out, artifact)
	}
	return out, nil
}

// Upload puts one file and returns its artifact record.
func (u *Uploader) Upload(ctx context.Context, upload Upload) (qaschema.Artifact, error) {
	if err := KeyWithinPrefix(u.prefix, upload.Key); err != nil {
		return qaschema.Artifact{}, err
	}
	if !upload.Kind.IsValid() {
		return qaschema.Artifact{}, fmt.Errorf("artifacts: unknown artifact kind %q", upload.Kind)
	}
	if !u.now().Before(u.expiresAt) {
		return qaschema.Artifact{}, fmt.Errorf("%w at %s", ErrPrefixExpired, u.expiresAt.Format(time.RFC3339))
	}

	file, err := os.Open(upload.Path) //nolint:gosec // the path is one the daemon itself handed the executor
	if err != nil {
		return qaschema.Artifact{}, fmt.Errorf("artifacts: open %s: %w", upload.Path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return qaschema.Artifact{}, fmt.Errorf("artifacts: stat %s: %w", upload.Path, err)
	}
	digest, err := hashFile(file)
	if err != nil {
		return qaschema.Artifact{}, err
	}

	target, err := u.urlFor(upload.Key)
	if err != nil {
		return qaschema.Artifact{}, err
	}
	contentType := upload.ContentType
	if contentType == "" {
		contentType = guessContentType(upload.Key)
	}

	if err := u.put(ctx, target, file, info.Size(), contentType, upload.Key); err != nil {
		return qaschema.Artifact{}, err
	}

	size := int(info.Size())
	id := upload.ID
	if id == "" {
		id = newArtifactID(digest)
	}
	return qaschema.Artifact{
		ID:          id,
		Kind:        upload.Kind,
		Key:         upload.Key,
		ContentType: &contentType,
		SizeBytes:   &size,
		Sha256:      &digest,
	}, nil
}

// put performs the PUT with bounded retries. Only failures that a retry can
// plausibly fix are retried; a 403 from an expired signature is reported
// immediately rather than retried three times against a URL that will keep
// saying 403.
func (u *Uploader) put(ctx context.Context, target string, body *os.File, size int64, contentType, key string) error {
	var lastErr error
	for attempt := 1; attempt <= u.attempts; attempt++ {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("artifacts: rewind %s: %w", body.Name(), err)
		}

		// NopCloser because the transport closes the request body, and this
		// file has to survive to be re-sent on the next attempt.
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, io.NopCloser(body))
		if err != nil {
			return fmt.Errorf("artifacts: build request: %w", err)
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", contentType)

		resp, err := u.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("artifacts: upload %s: %w", key, ctx.Err())
			}
			lastErr = fmt.Errorf("artifacts: upload %s: %w", key, err)
		} else {
			status := resp.StatusCode
			// The body is drained so the connection can be reused, and capped
			// so a hostile endpoint cannot make the daemon buffer megabytes of
			// error text.
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()

			if status >= 200 && status < 300 {
				u.logger.Debug("artifact uploaded", "key", key, "bytes", size, "attempt", attempt)
				return nil
			}
			lastErr = fmt.Errorf("artifacts: upload %s: storage returned %d: %s", key, status, strings.TrimSpace(string(snippet)))
			if !retryableStatus(status) {
				return lastErr
			}
		}

		if attempt == u.attempts {
			break
		}
		if err := u.sleep(ctx, u.jitteredBackoff(attempt)); err != nil {
			return fmt.Errorf("artifacts: upload %s: %w", key, err)
		}
		u.logger.Warn("retrying artifact upload", "key", key, "attempt", attempt, "error", lastErr)
	}
	return lastErr
}

// urlFor appends the object key to the presigned base, preserving the query
// string that carries the signature. The base is a prefix URL, not a
// per-object one: the backend signs the run's prefix once and the daemon
// writes every object of that run below it (ADR-002).
func (u *Uploader) urlFor(key string) (string, error) {
	if err := KeyWithinPrefix(u.prefix, key); err != nil {
		return "", err
	}
	target := *u.base
	// url.JoinPath escapes each element, which is what keeps a key segment
	// from injecting a query string or another path.
	joined := target.JoinPath(strings.Split(key, "/")...)
	joined.RawQuery = u.base.RawQuery
	return joined.String(), nil
}

func (u *Uploader) jitteredBackoff(attempt int) time.Duration {
	d := u.backoff << (attempt - 1)
	// Full jitter: two objects that fail against the same overloaded storage
	// endpoint must not retry in lockstep.
	return time.Duration(rand.Int64N(int64(d)) + int64(d)/2) //nolint:gosec // jitter, not a secret
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

func hashFile(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("artifacts: rewind %s: %w", f.Name(), err)
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("artifacts: hash %s: %w", f.Name(), err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// newArtifactID derives the run-local handle from the content digest, so the
// same file uploaded twice by a resumed run gets the same id.
func newArtifactID(digest string) string {
	return "art-" + digest[:16]
}

func guessContentType(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".zip":
		return "application/zip"
	case ".json", ".har":
		return "application/json"
	case ".webm":
		return "video/webm"
	case ".log", ".txt":
		return "text/plain; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	}
	if byExt := mime.TypeByExtension(filepath.Ext(key)); byExt != "" {
		return byExt
	}
	return "application/octet-stream"
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
