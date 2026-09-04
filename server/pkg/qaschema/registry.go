package qaschema

import (
	"embed"
	"fmt"
	"strings"
	"sync"
)

// The contract documents travel with the binary. A daemon on a customer's
// machine has no repository checkout to read them from, and a backend that
// validated against a schema file on disk could be pointed at a different one.
//
//go:embed schemas/*.schema.json
var schemaFS embed.FS

// UnknownSchemaError is returned when a caller names a contract that does not
// exist in this build. It is deliberately not a fallback to "no validation".
type UnknownSchemaError struct{ ID string }

func (e *UnknownSchemaError) Error() string {
	return fmt.Sprintf("unknown schema %q; known ids: %s", e.ID, strings.Join(SchemaIDs, ", "))
}

var (
	loadMu   sync.Mutex
	loaded   = map[string]any{}
	byURI    map[string]string
	byURIOne sync.Once
)

func uriIndex() map[string]string {
	byURIOne.Do(func() {
		byURI = make(map[string]string, len(schemaURIs))
		for id, uri := range schemaURIs {
			byURI[uri] = id
		}
	})
	return byURI
}

// IsSchemaID reports whether id names a contract in this build.
func IsSchemaID(id string) bool {
	_, ok := schemaFiles[id]
	return ok
}

// SchemaURI returns the $id a contract is referenced by.
func SchemaURI(id string) (string, bool) {
	uri, ok := schemaURIs[id]
	return uri, ok
}

// SchemaDocument returns the parsed contract document, checking on first use
// that it only uses keywords this validator implements.
func SchemaDocument(id string) (any, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	return loadLocked(id)
}

func loadLocked(id string) (any, error) {
	if document, ok := loaded[id]; ok {
		return document, nil
	}
	file, ok := schemaFiles[id]
	if !ok {
		return nil, &UnknownSchemaError{ID: id}
	}
	raw, err := schemaFS.ReadFile("schemas/" + file)
	if err != nil {
		return nil, fmt.Errorf("read embedded schema %s: %w", file, err)
	}
	document, err := DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("parse schema %s: %w", id, err)
	}
	if err := AssertSupported(document, schemaURIs[id]); err != nil {
		return nil, fmt.Errorf("schema %s: %w", id, err)
	}
	loaded[id] = document
	return document, nil
}

func resolver(uri string) (any, bool) {
	id, ok := uriIndex()[uri]
	if !ok {
		return nil, false
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	document, err := loadLocked(id)
	if err != nil {
		return nil, false
	}
	return document, true
}

// Validate checks an already-decoded value against a contract.
//
// Decode the value with DecodeJSON, not encoding/json's default: "integer"
// cannot be told from "number" once every value is a float64.
//
// A non-nil error means the schema or the id is wrong. Invalid data is a
// ValidationResult with Valid false, one entry per failing field.
func Validate(id string, instance any) (ValidationResult, error) {
	document, err := SchemaDocument(id)
	if err != nil {
		return ValidationResult{}, err
	}
	return ValidateWith(document, schemaURIs[id], instance, resolver)
}

// ValidateJSON decodes raw JSON and validates it. Malformed JSON is reported as
// a validation error at the document root rather than as a Go error: to the
// caller, "the daemon sent us something unparsable" and "the daemon sent us the
// wrong shape" are the same class of problem.
func ValidateJSON(id string, data []byte) (ValidationResult, error) {
	if !IsSchemaID(id) {
		return ValidationResult{}, &UnknownSchemaError{ID: id}
	}
	instance, err := DecodeJSON(data)
	if err != nil {
		return ValidationResult{
			Valid: false,
			Errors: []ValidationError{
				newError("", schemaURIs[id], "parse", "not valid JSON: "+err.Error()),
			},
		}, nil
	}
	return Validate(id, instance)
}

// MustBeValid is the shape most callers want at a trust boundary: either the
// document is good, or there is an error naming every field that is not.
func MustBeValid(id string, data []byte) error {
	result, err := ValidateJSON(id, data)
	if err != nil {
		return err
	}
	if result.Valid {
		return nil
	}
	parts := make([]string, len(result.Errors))
	for i, e := range result.Errors {
		parts[i] = e.String()
	}
	return fmt.Errorf("%s: %s", id, strings.Join(parts, "; "))
}
