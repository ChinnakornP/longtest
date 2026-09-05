package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// MinSecretLen is the shortest value the scrubber will accept.
//
// Registering a two-character password would replace every occurrence of those
// two characters in every log line, destroying the logs and telling an
// observer exactly how short the secret is. A value below this bound is a
// configuration error, not something to silently accept.
const MinSecretLen = 6

// ErrSecretTooShort is returned by [Scrubber.Add] for a value under
// [MinSecretLen].
var ErrSecretTooShort = errors.New("security: secret is too short to redact safely")

// Scrubber removes registered secret values from anything on its way out of
// the daemon.
//
// Everything a run emits passes through one of these: prompt files, workspace
// JSON, the run log, `run.event` frames, and artifact bodies. The point is not
// that any single one of those is expected to contain a credential — it is
// that the target app echoes what you type, so a screenshot caption, a form
// dump, a 500-page stack trace or a network log will eventually contain one,
// and the only maintainable answer is a single choke point rather than a
// discipline every future call site has to remember.
//
// A Scrubber is safe for concurrent use.
type Scrubber struct {
	mu sync.RWMutex
	// variants maps every encoding of every registered secret to its
	// replacement token. Longest-first ordering is applied at match time so a
	// secret that contains another secret redacts as one unit.
	variants map[string]string
	ordered  []string
	longest  int
}

// NewScrubber returns an empty Scrubber. An empty Scrubber is valid and
// passes input through unchanged.
func NewScrubber() *Scrubber {
	return &Scrubber{variants: map[string]string{}}
}

// Add registers a secret value.
//
// It also registers the encodings the value takes on as it travels: the URL
// escaping a form POST applies, the JSON string escaping a workspace file
// applies, and base64 at all three phase alignments, which is how a credential
// survives inside a Basic auth header or a data: URL. Without those, a
// scrubber gives false assurance: the plaintext is gone from the log and the
// percent-encoded copy two lines down is not.
func (s *Scrubber) Add(secret string) error {
	if len(secret) < MinSecretLen {
		return fmt.Errorf("%w: %d bytes, minimum %d", ErrSecretTooShort, len(secret), MinSecretLen)
	}
	token := RedactionToken(secret)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range encodings(secret) {
		if len(v) < MinSecretLen {
			// A short encoding of a long secret would over-match; skip it
			// rather than corrupt unrelated output.
			continue
		}
		if _, ok := s.variants[v]; ok {
			continue
		}
		s.variants[v] = token
		s.ordered = append(s.ordered, v)
		if len(v) > s.longest {
			s.longest = len(v)
		}
	}
	sort.Slice(s.ordered, func(i, j int) bool {
		if len(s.ordered[i]) != len(s.ordered[j]) {
			return len(s.ordered[i]) > len(s.ordered[j])
		}
		return s.ordered[i] < s.ordered[j]
	})
	return nil
}

// AddAll registers several secrets, reporting the first failure.
func (s *Scrubber) AddAll(secrets ...string) error {
	for _, v := range secrets {
		if err := s.Add(v); err != nil {
			return err
		}
	}
	return nil
}

// RedactionToken is the placeholder a value is replaced with.
//
// It carries a truncated SHA-256 of the secret so that two occurrences of the
// same credential are recognisably the same in a log — which is most of the
// debugging value of seeing the credential at all — without disclosing it. 8
// hex characters is 32 bits: enough to correlate within one run, far too
// little to attack the preimage of a real password.
func RedactionToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "[redacted:" + hex.EncodeToString(sum[:4]) + "]"
}

// encodings returns the forms a secret may appear in downstream.
func encodings(secret string) []string {
	out := []string{secret}
	add := func(v string) {
		if v == "" || v == secret {
			return
		}
		for _, existing := range out {
			if existing == v {
				return
			}
		}
		out = append(out, v)
	}

	add(url.QueryEscape(secret))
	add(url.PathEscape(secret))
	if j, err := json.Marshal(secret); err == nil {
		add(strings.Trim(string(j), `"`))
	}
	// Base64 of a substring is a substring of the base64 only when the offsets
	// align, so a secret embedded in a larger base64 blob (an Authorization
	// header, a data: URL) matches at exactly one of three phases.
	for pad := 0; pad < 3; pad++ {
		padded := strings.Repeat("\x00", pad) + secret
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			e := enc.EncodeToString([]byte(padded))
			// Drop the leading characters that encode the padding bytes, and
			// the trailing group that may be contaminated by what follows.
			start := (pad*8 + 5) / 6
			if len(e) > start+4 {
				add(e[start : len(e)-4])
			}
		}
	}
	return out
}

// String returns in with every registered secret replaced.
func (s *Scrubber) String(in string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.ordered) == 0 {
		return in
	}
	out := in
	for _, v := range s.ordered {
		if strings.Contains(out, v) {
			out = strings.ReplaceAll(out, v, s.variants[v])
		}
	}
	return out
}

// Bytes returns b with every registered secret replaced. The input slice is
// not modified.
func (s *Scrubber) Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return []byte(s.String(string(b)))
}

// Contains reports whether in still holds a registered secret. Tests and the
// pre-upload artifact check use it as an assertion rather than a transform.
func (s *Scrubber) Contains(in string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.ordered {
		if strings.Contains(in, v) {
			return true
		}
	}
	return false
}

// JSON scrubs every string in a JSON document, keys included, and returns the
// re-encoded document.
//
// Going through the structure rather than the raw bytes matters because JSON
// escaping is not unique: a workspace file may hold a password as
// "pass", which no substring search over the encoded bytes will find.
func (s *Scrubber) JSON(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("security: scrub json: %w", err)
	}
	return json.Marshal(s.value(v))
}

func (s *Scrubber) value(v any) any {
	switch t := v.(type) {
	case string:
		return s.String(t)
	case []any:
		for i := range t {
			t[i] = s.value(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[s.String(k)] = s.value(val)
		}
		return out
	default:
		return v
	}
}

// Writer wraps w so that everything written through it is scrubbed.
//
// It buffers the tail of each write, because a secret can straddle two calls —
// a log line assembled in pieces, or a CLI's stdout arriving in whatever chunk
// sizes the pipe produced. Callers must Close to flush that tail.
func (s *Scrubber) Writer(w io.Writer) io.WriteCloser {
	return &scrubWriter{s: s, w: w}
}

type scrubWriter struct {
	s      *Scrubber
	w      io.Writer
	buf    []byte
	closed bool
}

func (sw *scrubWriter) Write(p []byte) (int, error) {
	if sw.closed {
		return 0, errors.New("security: write to a closed scrub writer")
	}
	sw.buf = append(sw.buf, p...)

	sw.s.mu.RLock()
	hold := sw.s.longest
	sw.s.mu.RUnlock()
	if hold > 0 {
		hold--
	}

	// Emit everything except a tail long enough to hide a split secret, and
	// stop at the last newline so partial lines are not flushed mid-token.
	cut := len(sw.buf) - hold
	if cut <= 0 {
		return len(p), nil
	}
	if nl := lastNewline(sw.buf[:cut]); nl >= 0 {
		cut = nl + 1
	}
	if _, err := sw.w.Write(sw.s.Bytes(sw.buf[:cut])); err != nil {
		return 0, err
	}
	sw.buf = append(sw.buf[:0], sw.buf[cut:]...)
	return len(p), nil
}

func (sw *scrubWriter) Close() error {
	if sw.closed {
		return nil
	}
	sw.closed = true
	if len(sw.buf) == 0 {
		return nil
	}
	_, err := sw.w.Write(sw.s.Bytes(sw.buf))
	sw.buf = nil
	return err
}

func lastNewline(b []byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == '\n' {
			return i
		}
	}
	return -1
}
