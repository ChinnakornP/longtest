// The tests live in the package rather than qaschema_test on purpose: this
// package is mirrored verbatim into the daemon module, so it must not name its
// own import path.
package qaschema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureRoot is the same directory the TypeScript suite reads. Both modules sit
// three levels below the repository root, so the path works from either copy of
// this package.
const fixtureRoot = "../../../packages/qa-schema/fixtures"

type expectedError struct {
	InstancePath string `json:"instancePath"`
	Keyword      string `json:"keyword"`
	Message      string `json:"message"`
}

type expectation struct {
	SchemaID string          `json:"schemaId"`
	Valid    bool            `json:"valid"`
	Errors   []expectedError `json:"errors"`
}

func loadExpectations(t *testing.T) map[string]expectation {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, "expectations.json"))
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var expectations map[string]expectation
	if err := json.Unmarshal(raw, &expectations); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	if len(expectations) == 0 {
		t.Fatal("expectations.json is empty")
	}
	return expectations
}

// TestFixturesMatchTypeScript is the cross-language contract test.
//
// expectations.json is generated from the TypeScript validator and reviewed by
// hand. Asserting the Go validator against it, error for error, is what turns
// "both validators agree" from an intention into something CI can fail on: a
// backend that rejected a frame the executor accepted would be a bug nobody
// could see from either side alone.
func TestFixturesMatchTypeScript(t *testing.T) {
	for key, want := range loadExpectations(t) {
		t.Run(key, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(fixtureRoot, filepath.FromSlash(key)))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			result, err := ValidateJSON(want.SchemaID, data)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			if result.Valid != want.Valid {
				t.Fatalf("valid = %v, want %v (errors: %v)", result.Valid, want.Valid, result.Errors)
			}
			if len(result.Errors) != len(want.Errors) {
				t.Fatalf("got %d errors, want %d\n got: %+v\nwant: %+v",
					len(result.Errors), len(want.Errors), result.Errors, want.Errors)
			}
			for i, got := range result.Errors {
				expected := want.Errors[i]
				if got.InstancePath != expected.InstancePath || got.Keyword != expected.Keyword || got.Message != expected.Message {
					t.Errorf("error %d:\n got  %s | %s | %s\n want %s | %s | %s",
						i, got.InstancePath, got.Keyword, got.Message,
						expected.InstancePath, expected.Keyword, expected.Message)
				}
			}
		})
	}
}

// TestFixtureCoverage keeps the fixture set honest: every contract needs both
// buckets populated, and a document filed under valid/ has to actually validate.
func TestFixtureCoverage(t *testing.T) {
	expectations := loadExpectations(t)

	for _, id := range SchemaIDs {
		name := id[:strings.LastIndex(id, "@")]
		for _, bucket := range []string{"valid", "invalid"} {
			entries, err := os.ReadDir(filepath.Join(fixtureRoot, name, bucket))
			if err != nil {
				t.Fatalf("%s/%s: %v", name, bucket, err)
			}
			count := 0
			for _, entry := range entries {
				if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
					continue
				}
				count++
				key := name + "/" + bucket + "/" + entry.Name()
				want, ok := expectations[key]
				if !ok {
					t.Errorf("%s has no entry in expectations.json (run pnpm --filter @qa/schema run gen:expectations)", key)
					continue
				}
				if want.Valid != (bucket == "valid") {
					t.Errorf("%s is filed under %s/ but expectations say valid=%v", key, bucket, want.Valid)
				}
			}
			if count < 3 {
				t.Errorf("%s/%s has %d fixtures, want at least 3", name, bucket, count)
			}
		}
	}
}

func TestSchemaIDsAreSortedAndComplete(t *testing.T) {
	want := []string{
		"application-map@1",
		"daemon-envelope@1",
		"execution-result@1",
		"finding@1",
		"test-case@1",
		"test-plan@1",
	}
	if !slices.Equal(SchemaIDs, want) {
		t.Fatalf("SchemaIDs = %v, want %v", SchemaIDs, want)
	}
	for _, id := range SchemaIDs {
		if !IsSchemaID(id) {
			t.Errorf("IsSchemaID(%q) = false", id)
		}
		uri, ok := SchemaURI(id)
		if !ok || !strings.HasPrefix(uri, "https://qa.local/schema/") {
			t.Errorf("SchemaURI(%q) = %q, %v", id, uri, ok)
		}
		major := id[strings.LastIndex(id, "@")+1:]
		if !strings.HasPrefix(ContractVersions[id], major+".") {
			t.Errorf("ContractVersions[%q] = %q, which is not in major %s", id, ContractVersions[id], major)
		}
		if _, err := SchemaDocument(id); err != nil {
			t.Errorf("SchemaDocument(%q): %v", id, err)
		}
	}
}

func TestUnknownSchemaIsAnError(t *testing.T) {
	if IsSchemaID("test-case@2") {
		t.Error("IsSchemaID accepted a version this build does not have")
	}
	var unknown *UnknownSchemaError
	_, err := Validate("test-case@2", map[string]any{})
	if err == nil {
		t.Fatal("Validate accepted an unknown schema id")
	}
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want UnknownSchemaError", err)
	}
	if !strings.Contains(unknown.Error(), "known ids") {
		t.Errorf("error message should list the ids that do exist, got %q", unknown.Error())
	}
}
