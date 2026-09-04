package httpx

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Page is a bounded list window. Every list endpoint takes one, so no endpoint
// can be asked for an unbounded result set.
type Page struct {
	Limit  int32
	Offset int32
}

// DefaultPageLimit and MaxPageLimit bound every list endpoint in this API.
const (
	DefaultPageLimit int32 = 50
	MaxPageLimit     int32 = 200
)

// PageFrom reads ?limit= and ?offset=.
//
// An absent value is the default; an out-of-range or unparsable one is a 400
// rather than a silent clamp, because a client asking for 10000 rows and
// receiving 200 without being told has no way to notice it is missing data.
func PageFrom(r *http.Request) (Page, error) {
	limit, err := queryInt32(r, "limit", DefaultPageLimit, 1, MaxPageLimit)
	if err != nil {
		return Page{}, err
	}
	offset, err := queryInt32(r, "offset", 0, 0, 1<<30)
	if err != nil {
		return Page{}, err
	}
	return Page{Limit: limit, Offset: offset}, nil
}

func queryInt32(r *http.Request, name string, fallback, minimum, maximum int32) (int32, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || int32(value) < minimum || int32(value) > maximum {
		return 0, BadRequest("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return int32(value), nil
}

// QueryUUIDPtr reads an optional uuid query parameter, e.g. ?projectId=.
func QueryUUIDPtr(r *http.Request, name string) (*uuid.UUID, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil //nolint:nilnil // "absent" is a distinct, expected outcome here
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, BadRequest("%s must be a uuid", name)
	}
	return &id, nil
}
