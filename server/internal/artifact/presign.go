package artifact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SigV4 query-string presigning, implemented here rather than pulled in with
// the AWS SDK.
//
// The whole surface the backend needs is "sign one PUT and one GET for one
// object", which is the ~100 lines below and is exercised against the vectors
// in presign_test.go. The SDK would add a few dozen modules to a service that
// otherwise depends on pgx and nothing else, and would still need this
// package's key rules on top.

const (
	algorithm       = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	amzDateFormat   = "20060102T150405Z"
	scopeDateFormat = "20060102"

	// maxPresignTTL is the ceiling SigV4 itself imposes on a query-string
	// signature. The contract's own cap (six hours) is lower and is applied by
	// Config.
	maxPresignTTL = 7 * 24 * time.Hour
)

// Credentials are the static access key pair the backend signs with. They come
// from the environment and are never logged or rendered.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	// SessionToken is set only when the deployment uses temporary
	// credentials; it becomes an extra signed query parameter.
	SessionToken string
}

// presigner turns (method, key, ttl) into a signed URL against one bucket.
type presigner struct {
	endpoint *url.URL
	region   string
	bucket   string
	creds    Credentials
	// pathStyle addresses the bucket as {endpoint}/{bucket}/{key}. MinIO and a
	// bare IP endpoint require it; a real S3 deployment prefers the virtual
	// host form, which is what an empty value selects.
	pathStyle bool
}

// presign returns a URL that authorises exactly one request: this method, on
// this key, until now+ttl.
func (p *presigner) presign(method, key string, now time.Time, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", fmt.Errorf("presign ttl must be between 1s and %s, got %s", maxPresignTTL, ttl)
	}

	host, canonicalPath := p.address(key)
	now = now.UTC()
	amzDate := now.Format(amzDateFormat)
	scope := strings.Join([]string{now.Format(scopeDateFormat), p.region, "s3", "aws4_request"}, "/")

	query := url.Values{
		"X-Amz-Algorithm":     {algorithm},
		"X-Amz-Credential":    {p.creds.AccessKeyID + "/" + scope},
		"X-Amz-Date":          {amzDate},
		"X-Amz-Expires":       {strconv.Itoa(int(ttl.Seconds()))},
		"X-Amz-SignedHeaders": {"host"},
	}
	if p.creds.SessionToken != "" {
		query.Set("X-Amz-Security-Token", p.creds.SessionToken)
	}

	canonicalRequest := strings.Join([]string{
		method,
		canonicalPath,
		canonicalQuery(query),
		"host:" + host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	query.Set("X-Amz-Signature", hex.EncodeToString(hmacSHA256(p.signingKey(now), []byte(stringToSign))))

	// The URL is rebuilt from the host the signature committed to, not from the
	// configured endpoint: in virtual-host style those differ (the bucket moves
	// into the hostname), and a URL whose Host header does not match the signed
	// one is rejected by the store with a signature mismatch.
	signed := url.URL{Scheme: p.endpoint.Scheme, Host: host}
	return signed.String() + canonicalPath + "?" + canonicalQuery(query), nil
}

// address returns the Host header the signature commits to and the canonical,
// already-escaped request path.
func (p *presigner) address(key string) (host, path string) {
	host = p.endpoint.Host
	if !p.pathStyle {
		return p.bucket + "." + host, "/" + escapePath(key)
	}
	return host, "/" + escapePath(p.bucket) + "/" + escapePath(key)
}

// signingKey derives the date/region/service-scoped key. It is recomputed per
// request rather than cached: it is four HMACs, and caching it would mean
// holding derived key material for the lifetime of the process.
func (p *presigner) signingKey(now time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+p.creds.SecretAccessKey), []byte(now.Format(scopeDateFormat)))
	k = hmacSHA256(k, []byte(p.region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// canonicalQuery renders the query string the way SigV4 requires: sorted by
// key, every octet escaped per RFC 3986. net/url's Encode is close but escapes
// a space as "+", which does not verify.
func canonicalQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		for _, v := range values[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(escape(k))
			b.WriteByte('=')
			b.WriteString(escape(v))
		}
	}
	return b.String()
}

// escapePath escapes an object key for the canonical URI, leaving the "/" that
// separates key segments intact.
func escapePath(key string) string {
	segments := strings.Split(key, "/")
	for i, s := range segments {
		segments[i] = escape(s)
	}
	return strings.Join(segments, "/")
}

// escape is RFC 3986 unreserved-set percent-encoding: everything except
// A-Z a-z 0-9 - _ . ~ is escaped, uppercase hex.
func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
