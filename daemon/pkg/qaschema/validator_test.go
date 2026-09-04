// Mirrored from server/pkg/qaschema/validator_test.go by packages/qa-schema/scripts/generate.mjs.
// DO NOT EDIT: change the server copy and run `make gen`.

package qaschema

import (
	"errors"
	"strings"
	"testing"
)

const testURI = "https://qa.local/schema/test/1"

// check validates instance against schema, with extra documents available to
// $ref. It mirrors the helper of the same name in test/validator.test.ts; the
// two suites are deliberately the same cases in the same order.
func check(t *testing.T, schema string, instance string, extra map[string]string) ValidationResult {
	t.Helper()
	documents := map[string]any{}
	decode := func(text string) any {
		value, err := DecodeJSON([]byte(text))
		if err != nil {
			t.Fatalf("fixture is not JSON: %v", err)
		}
		return value
	}
	root := decode(schema)
	documents[testURI] = root
	for uri, text := range extra {
		documents[uri] = decode(text)
	}
	result, err := ValidateWith(root, testURI, decode(instance), func(uri string) (any, bool) {
		document, ok := documents[uri]
		return document, ok
	})
	if err != nil {
		t.Fatalf("unexpected schema error: %v", err)
	}
	return result
}

func assertSupportedText(t *testing.T, schema string) error {
	t.Helper()
	document, err := DecodeJSON([]byte(schema))
	if err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return AssertSupported(document, testURI)
}

func TestAssertSupportedRejectsUnknownKeywords(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{
			// The whole reason this validator is hand-rolled: a library would
			// accept unevaluatedProperties, ignore it, and quietly stop
			// enforcing it.
			name:   "keyword this validator does not implement",
			schema: `{"type":"object","unevaluatedProperties":false}`,
			want:   `unsupported schema keyword "unevaluatedProperties"`,
		},
		{
			name:   "keyword nested inside an applicator",
			schema: `{"allOf":[{"properties":{"a":{"contains":{}}}}]}`,
			want:   `unsupported schema keyword "contains"`,
		},
		{
			name:   "lookahead RE2 cannot compile",
			schema: `{"type":"string","pattern":"^(?=.*a)"}`,
			want:   "RE2 cannot compile",
		},
		{
			name:   "backreference RE2 cannot compile",
			schema: `{"type":"string","pattern":"(a)\\1"}`,
			want:   "RE2 cannot compile",
		},
		{
			name:   "format with no shared definition",
			schema: `{"type":"string","format":"ipv6"}`,
			want:   `unsupported format "ipv6"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := assertSupportedText(t, tc.schema)
			if err == nil {
				t.Fatal("expected a schema error")
			}
			var schemaErr *SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("error = %T, want *SchemaError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAssertSupportedAllowsExtensions(t *testing.T) {
	if err := assertSupportedText(t, `{"type":"string","x-go-type":"string"}`); err != nil {
		t.Fatalf("x- extensions are annotations for the generator: %v", err)
	}
}

func TestTypeKeyword(t *testing.T) {
	tests := []struct {
		name     string
		schema   string
		instance string
		valid    bool
	}{
		{"integer accepts a whole number", `{"type":"integer"}`, `1`, true},
		{"integer accepts a whole float, as the spec says", `{"type":"integer"}`, `1.0`, true},
		{"integer rejects a fraction", `{"type":"integer"}`, `1.5`, false},
		{"number accepts an integer", `{"type":"number"}`, `1`, true},
		{"nullable union accepts null", `{"type":["integer","null"]}`, `null`, true},
		{"nullable union accepts the base type", `{"type":["integer","null"]}`, `3`, true},
		{"nullable union rejects anything else", `{"type":["integer","null"]}`, `"x"`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := check(t, tc.schema, tc.instance, nil).Valid; got != tc.valid {
				t.Errorf("valid = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestTypeIsAGate(t *testing.T) {
	result := check(t, `{"type":"string","minLength":5,"pattern":"^a"}`, `42`, nil)
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(result.Errors), result.Errors)
	}
	want := ValidationError{
		InstancePath: "",
		SchemaPath:   testURI + "/type",
		Keyword:      "type",
		Message:      "expected string, got number",
	}
	if result.Errors[0] != want {
		t.Errorf("error = %+v, want %+v", result.Errors[0], want)
	}
}

func TestObjectErrorsAreStableAndLocated(t *testing.T) {
	schema := `{
		"type":"object",
		"additionalProperties":false,
		"required":["a"],
		"properties":{"a":{"type":"string"},"b":{"type":"integer"}}
	}`

	t.Run("names the missing property and the stray one", func(t *testing.T) {
		result := check(t, schema, `{"b":1,"c":true}`, nil)
		got := make([][2]string, len(result.Errors))
		for i, e := range result.Errors {
			got[i] = [2]string{e.InstancePath, e.Keyword}
		}
		want := [][2]string{{"", "required"}, {"/c", "additionalProperties"}}
		if len(got) != len(want) {
			t.Fatalf("errors = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("error %d = %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("order does not depend on map iteration order", func(t *testing.T) {
		// Go randomises map iteration; without the explicit sort in
		// validateObject this test fails roughly every other run, and the Go
		// and TypeScript error lists would stop matching.
		first := check(t, schema, `{"z":1,"c":2,"b":"no"}`, nil)
		for i := 0; i < 20; i++ {
			again := check(t, schema, `{"b":"no","c":2,"z":1}`, nil)
			if len(again.Errors) != len(first.Errors) {
				t.Fatalf("run %d produced %d errors, first run produced %d", i, len(again.Errors), len(first.Errors))
			}
			for j := range first.Errors {
				if again.Errors[j] != first.Errors[j] {
					t.Fatalf("run %d error %d = %+v, want %+v", i, j, again.Errors[j], first.Errors[j])
				}
			}
		}
		paths := make([]string, len(first.Errors))
		for i, e := range first.Errors {
			paths[i] = e.InstancePath
		}
		if strings.Join(paths, ",") != ",/b,/c,/z" {
			t.Errorf("paths = %v, want [ /b /c /z]", paths)
		}
	})

	t.Run("escapes a JSON Pointer token", func(t *testing.T) {
		result := check(t, `{"type":"object","additionalProperties":false}`, `{"a/b~c":1}`, nil)
		if len(result.Errors) != 1 || result.Errors[0].InstancePath != "/a~1b~0c" {
			t.Errorf("errors = %+v, want one error at /a~1b~0c", result.Errors)
		}
	})
}

func TestOneOfNamesTheFailingField(t *testing.T) {
	schema := `{"oneOf":[
		{"type":"object","required":["ref"],"additionalProperties":false,"properties":{"ref":{"type":"string"}}},
		{"type":"object","required":["locator","unstable"],"additionalProperties":false,
		 "properties":{"locator":{"type":"string"},"unstable":{"const":true}}}
	]}`
	result := check(t, schema, `{"locator":"#a"}`, nil)
	if result.Valid {
		t.Fatal("a raw locator without unstable:true must be rejected")
	}
	if !strings.Contains(result.Errors[0].Message, `missing required property "unstable"`) {
		t.Errorf("message = %q, want it to name the missing field", result.Errors[0].Message)
	}
}

func TestOneOfRejectsAnAmbiguousMatch(t *testing.T) {
	result := check(t, `{"oneOf":[{"type":"string"},{"minLength":0}]}`, `"x"`, nil)
	if result.Valid || !strings.Contains(result.Errors[0].Message, "matches 2 of the 2 allowed shapes") {
		t.Errorf("errors = %+v, want an ambiguity complaint", result.Errors)
	}
}

func TestIfThen(t *testing.T) {
	schema := `{
		"type":"object",
		"properties":{"kind":{"type":"string"}},
		"if":{"required":["kind"],"properties":{"kind":{"const":"role"}}},
		"then":{"required":["kind","name"]}
	}`
	tests := []struct {
		instance string
		valid    bool
	}{
		{`{"kind":"css"}`, true},
		{`{"kind":"role"}`, false},
		{`{"kind":"role","name":"Save"}`, true},
	}
	for _, tc := range tests {
		if got := check(t, schema, tc.instance, nil).Valid; got != tc.valid {
			t.Errorf("%s: valid = %v, want %v", tc.instance, got, tc.valid)
		}
	}
}

func TestRefIntoAnotherDocument(t *testing.T) {
	const other = "https://qa.local/schema/other/1"
	result := check(t,
		`{"$ref":"`+other+`#/$defs/Positive"}`,
		`-1`,
		map[string]string{other: `{"$defs":{"Positive":{"type":"integer","minimum":0}}}`},
	)
	if len(result.Errors) != 1 {
		t.Fatalf("errors = %+v, want one", result.Errors)
	}
	if result.Errors[0].Keyword != "minimum" || result.Errors[0].SchemaPath != other+"#/$defs/Positive/minimum" {
		t.Errorf("error = %+v, want it anchored in the referenced document", result.Errors[0])
	}
}

func TestSelfReferentialSchemaTerminates(t *testing.T) {
	schema := `{
		"$defs":{"Node":{"type":"object","required":["value"],
			"properties":{"value":{"type":"integer"},"next":{"$ref":"#/$defs/Node"}}}},
		"$ref":"#/$defs/Node"
	}`
	if !check(t, schema, `{"value":1,"next":{"value":2}}`, nil).Valid {
		t.Error("a well-formed recursive document should validate")
	}
	if check(t, schema, `{"value":1,"next":{"value":"two"}}`, nil).Valid {
		t.Error("recursion must not stop the nested error being found")
	}
}

func TestUnresolvableRefIsASchemaError(t *testing.T) {
	document, err := DecodeJSON([]byte(`{"$ref":"https://qa.local/schema/missing/1"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ValidateWith(document, testURI, map[string]any{}, func(string) (any, bool) { return nil, false })
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error = %v, want *SchemaError", err)
	}
}

func TestUniqueItemsPointsAtTheDuplicate(t *testing.T) {
	result := check(t, `{"type":"array","uniqueItems":true}`, `["a","b","a"]`, nil)
	want := ValidationError{
		InstancePath: "/2",
		SchemaPath:   testURI + "/uniqueItems",
		Keyword:      "uniqueItems",
		Message:      "duplicates the item at index 0",
	}
	if len(result.Errors) != 1 || result.Errors[0] != want {
		t.Errorf("errors = %+v, want %+v", result.Errors, want)
	}
}

func TestStringLengthCountsCodePoints(t *testing.T) {
	// Counting bytes here would disagree with JavaScript on anything outside
	// ASCII, and element labels come straight off real web pages.
	if !check(t, `{"type":"string","maxLength":3}`, `"กขค"`, nil).Valid {
		t.Error("three Thai code points should satisfy maxLength 3")
	}
	if check(t, `{"type":"string","maxLength":2}`, `"กขค"`, nil).Valid {
		t.Error("three code points should violate maxLength 2")
	}
}

func TestRegexSuppliedAsDataMustBePortable(t *testing.T) {
	schema := `{"type":"string","format":"regex"}`
	tests := []struct {
		value string
		valid bool
	}{
		{`"^/employees/[0-9]+$"`, true},
		{`"^(?=.*x)"`, false},
		{`"["`, false},
	}
	for _, tc := range tests {
		if got := check(t, schema, tc.value, nil).Valid; got != tc.valid {
			t.Errorf("%s: valid = %v, want %v", tc.value, got, tc.valid)
		}
	}
}

func TestMustBeValidNamesEveryBadField(t *testing.T) {
	err := MustBeValid("finding@1", []byte(`{"version":1,"testCaseId":"TC-1","stepIndex":0,
		"failureClass":"FLAKY","rootCause":"x","confidence":2,"evidence":[]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"/failureClass", "/confidence", "/evidence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err.Error(), want)
		}
	}
}

func TestValidateRejectsFloat64DecodedInput(t *testing.T) {
	// A caller who decodes with encoding/json's default gets float64 for every
	// number; that still works here because "integer" is defined as a whole
	// value, not as a Go type.
	result, err := Validate("finding@1", map[string]any{
		"version":      float64(1),
		"testCaseId":   "TC-1",
		"stepIndex":    float64(0),
		"failureClass": "TIMEOUT",
		"rootCause":    "the save never returned",
		"confidence":   0.5,
		"evidence":     []any{"trace-main"},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !result.Valid {
		t.Errorf("errors = %+v, want none", result.Errors)
	}
}
