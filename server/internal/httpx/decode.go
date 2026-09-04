package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// DefaultMaxBodyBytes bounds a JSON request body. Every payload this API
// accepts is a handful of short fields; anything larger is a mistake or an
// attempt to make the process allocate.
const DefaultMaxBodyBytes int64 = 1 << 20 // 1 MiB

// DecodeJSON reads exactly one JSON object from the request body into dst.
//
// It rejects, with a message the caller can act on:
//   - a Content-Type that is not application/json;
//   - a body over DefaultMaxBodyBytes;
//   - unknown fields, so a client that misspells "orgName" is told so instead
//     of silently creating an org called "";
//   - trailing content after the first JSON value.
//
// The error is always an *Error, so a handler can return it unchanged.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value in the same body means the client sent something other
	// than the single object this endpoint documents.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BadRequest("the request body must contain a single JSON object")
	}
	return nil
}

func requireJSONContentType(r *http.Request) error {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return &Error{
			Status:  http.StatusUnsupportedMediaType,
			Code:    CodeUnsupportedMediaType,
			Message: "Content-Type must be application/json",
		}
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return &Error{
			Status:  http.StatusUnsupportedMediaType,
			Code:    CodeUnsupportedMediaType,
			Message: "Content-Type must be application/json",
		}
	}
	return nil
}

// decodeError turns encoding/json's errors into messages that name the problem
// without echoing the body back (it may contain a password).
func decodeError(err error) error {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		maxErr    *http.MaxBytesError
	)

	switch {
	case errors.As(err, &maxErr):
		return &Error{
			Status:  http.StatusRequestEntityTooLarge,
			Code:    CodePayloadTooLarge,
			Message: fmt.Sprintf("the request body must be under %d bytes", maxErr.Limit),
		}
	case errors.As(err, &syntaxErr):
		return BadRequest("the request body is not valid JSON (at byte %d)", syntaxErr.Offset)
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return InvalidField(typeErr.Field, "must be a "+jsonTypeName(typeErr.Type.String()))
		}
		return BadRequest("the request body has the wrong shape")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return BadRequest("the request body is empty or truncated")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return InvalidField(field, "is not a field of this endpoint")
	default:
		// Anything else is a read failure on the connection, not a client
		// mistake we can name.
		return BadRequest("the request body could not be read")
	}
}

// jsonTypeName renders a Go type as something a JSON client recognises.
func jsonTypeName(goType string) string {
	switch goType {
	case "string":
		return "string"
	case "bool":
		return "boolean"
	case "map[string]interface {}", "map[string]string":
		return "object"
	default:
		if strings.HasPrefix(goType, "[]") {
			return "array"
		}
		if strings.HasPrefix(goType, "int") || strings.HasPrefix(goType, "float") ||
			strings.HasPrefix(goType, "uint") {
			return "number"
		}
		return goType
	}
}

// PathUUID reads a uuid path parameter, e.g. the {orgID} in
// /api/v1/orgs/{orgID}/members.
//
// A malformed id is a 400 and never reaches a query: uuid parsing is the only
// validation the database would otherwise do for us, and doing it here keeps a
// typo out of the error log.
func PathUUID(r *http.Request, name string) (uuid.UUID, error) {
	raw := r.PathValue(name)
	if raw == "" {
		return uuid.Nil, BadRequest("%s is required in the path", name)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, BadRequest("%s must be a uuid", name)
	}
	return id, nil
}
