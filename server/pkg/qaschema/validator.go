package qaschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValidationError is one failing keyword, located in both the instance and the
// schema. InstancePath is a JSON Pointer, so it can be pasted straight into a
// bug report or fed to a UI that highlights the offending field.
type ValidationError struct {
	InstancePath string `json:"instancePath"`
	SchemaPath   string `json:"schemaPath"`
	Keyword      string `json:"keyword"`
	Message      string `json:"message"`
}

func (e ValidationError) String() string {
	path := e.InstancePath
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s [%s]", path, e.Message, e.Keyword)
}

// ValidationResult is the outcome for one document. Invalid data is never an
// error: it is a result with errors, because the caller almost always wants to
// report every bad field at once rather than the first one.
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors"`
}

// SchemaError means the schema itself is unusable — a keyword this validator
// does not implement, an unresolvable $ref, a pattern Go cannot compile. It is
// a bug in the contract, never in the data.
type SchemaError struct{ msg string }

func (e *SchemaError) Error() string { return e.msg }

func schemaErrorf(format string, args ...any) *SchemaError {
	return &SchemaError{msg: fmt.Sprintf(format, args...)}
}

// Recursive descent over an arbitrarily nested schema reads far better without
// an error return on every frame, so a schema problem is raised as a panic with
// this wrapper type and converted back to an error at the package boundary.
// It never escapes: ValidateWith and AssertSupported both recover it.
type schemaPanic struct{ err *SchemaError }

func fail(format string, args ...any) {
	panic(schemaPanic{err: schemaErrorf(format, args...)})
}

var (
	annotationKeywords = []string{
		"$schema", "$id", "$anchor", "$comment",
		"title", "description", "default", "examples", "deprecated",
		"readOnly", "writeOnly",
	}
	applicatorSchema = map[string]bool{
		"if": true, "then": true, "else": true, "not": true,
		"items": true, "additionalProperties": true, "propertyNames": true,
	}
	applicatorMap  = map[string]bool{"properties": true, "patternProperties": true, "$defs": true}
	applicatorList = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true}

	assertionKeywords = []string{
		"$ref", "type", "const", "enum",
		"minLength", "maxLength", "pattern", "format",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
		"minItems", "maxItems", "uniqueItems",
		"required", "dependentRequired", "minProperties", "maxProperties",
	}

	knownKeywords = func() map[string]bool {
		known := map[string]bool{}
		for _, k := range annotationKeywords {
			known[k] = true
		}
		for _, k := range assertionKeywords {
			known[k] = true
		}
		for _, set := range []map[string]bool{applicatorSchema, applicatorMap, applicatorList} {
			for k := range set {
				known[k] = true
			}
		}
		return known
	}()

	// Constructs JavaScript accepts and RE2 does not. A pattern that only works
	// on one side of the wire is worse than no pattern at all, so it is rejected
	// when the schema loads.
	nonPortableRegex = regexp.MustCompile(`\(\?=|\(\?!|\(\?<|\\[1-9]`)

	formatPatterns = map[string]*regexp.Regexp{
		"uuid":      regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`),
		"date-time": regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})$`),
		"uri":       regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:[^\s]+$`),
	}
)

// ---------------------------------------------------------------------------
// value helpers
// ---------------------------------------------------------------------------

// DecodeJSON parses JSON into the representation this validator understands.
// Numbers are kept as json.Number so that "integer" means what the JSON says
// rather than what float64 rounding left behind.
func DecodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("decode json: trailing data after the top-level value")
	}
	return value, nil
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func schemaNumber(schema map[string]any, keyword string) (float64, bool) {
	raw, ok := schema[keyword]
	if !ok {
		return 0, false
	}
	return asNumber(raw)
}

// jsonTypeOf names the JSON type of a decoded value. "integer" is never
// returned; see matchesType.
func jsonTypeOf(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number, float64, int:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		_ = v
		return "unknown"
	}
}

func matchesType(value any, want string) bool {
	if want == "integer" {
		f, ok := asNumber(value)
		return ok && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f
	}
	return jsonTypeOf(value) == want
}

// formatNumber renders a number the way JavaScript's String() does, so error
// messages and canonical forms are byte-identical across the two validators.
func formatNumber(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "null"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// quoteJSON matches JSON.stringify for strings: no HTML escaping, which Go's
// json.Marshal would otherwise add.
func quoteJSON(s string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s); err != nil {
		return strconv.Quote(s)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// canonical renders a value so two values can be compared for JSON equality by
// comparing strings. Used by const, enum and uniqueItems.
func canonical(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case string:
		return quoteJSON(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = canonical(item)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := slices.Sorted(maps.Keys(v))
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = quoteJSON(key) + ":" + canonical(v[key])
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	if f, ok := asNumber(value); ok {
		return formatNumber(f)
	}
	return "null"
}

func quoteValue(value any) string {
	if s, ok := value.(string); ok {
		return quoteJSON(s)
	}
	return canonical(value)
}

func escapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

func unescapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
}

// ---------------------------------------------------------------------------
// schema support check
// ---------------------------------------------------------------------------

// AssertSupported walks a schema document and rejects any keyword this
// validator does not implement. Unknown "x-" extensions are annotations and
// pass through.
//
// This is what stops a contract from being half-enforced: a third-party
// validator ignores a keyword it does not know, which turns a tightened schema
// into a silently weaker one. Here the schema fails to load instead.
func AssertSupported(document any, uri string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			sp, ok := r.(schemaPanic)
			if !ok {
				panic(r)
			}
			err = sp.err
		}
	}()
	walkSchema(document, uri, "")
	return nil
}

func walkSchema(node any, uri, path string) {
	if _, ok := node.(bool); ok {
		return
	}
	schema, ok := asObject(node)
	if !ok {
		fail("%s%s: expected a schema object or boolean, got %s", uri, path, jsonTypeOf(node))
	}
	for _, key := range slices.Sorted(maps.Keys(schema)) {
		if strings.HasPrefix(key, "x-") {
			continue
		}
		if !knownKeywords[key] {
			fail("%s%s: unsupported schema keyword %q", uri, path, key)
		}
		value := schema[key]
		switch {
		case applicatorSchema[key]:
			walkSchema(value, uri, path+"/"+key)
		case applicatorMap[key]:
			members, ok := asObject(value)
			if !ok {
				fail("%s%s/%s: expected an object", uri, path, key)
			}
			for _, name := range slices.Sorted(maps.Keys(members)) {
				walkSchema(members[name], uri, path+"/"+key+"/"+escapePointer(name))
			}
		case applicatorList[key]:
			branches, ok := value.([]any)
			if !ok {
				fail("%s%s/%s: expected an array", uri, path, key)
			}
			for i, branch := range branches {
				walkSchema(branch, uri, fmt.Sprintf("%s/%s/%d", path, key, i))
			}
		case key == "pattern":
			assertPortableRegex(value, fmt.Sprintf("%s%s/pattern", uri, path))
		case key == "format":
			name, ok := value.(string)
			if !ok {
				fail("%s%s/format: must be a string", uri, path)
			}
			if name != "regex" && formatPatterns[name] == nil {
				fail("%s%s/format: unsupported format %q", uri, path, name)
			}
		}
	}
}

func assertPortableRegex(pattern any, where string) {
	value, ok := pattern.(string)
	if !ok {
		fail("%s: pattern must be a string", where)
	}
	if nonPortableRegex.MatchString(value) {
		fail("%s: pattern uses a construct Go's RE2 cannot compile (lookaround or backreference)", where)
	}
	if _, err := regexp.Compile(value); err != nil {
		fail("%s: pattern does not compile: %v", where, err)
	}
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

// Resolver returns the document registered under a schema URI.
type Resolver func(uri string) (any, bool)

type validationCtx struct {
	resolve Resolver
	// Guards a $ref cycle that would otherwise never reach an assertion.
	seen map[string]bool
	// Compiled patterns are reused across every element of a large array.
	patterns map[string]*regexp.Regexp
}

func (c *validationCtx) regex(pattern, where string) *regexp.Regexp {
	if compiled, ok := c.patterns[pattern]; ok {
		return compiled
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		fail("%s: pattern does not compile: %v", where, err)
	}
	c.patterns[pattern] = compiled
	return compiled
}

// ValidateWith validates instance against schema, resolving $ref through
// resolve. A returned error always means the schema is broken; invalid data
// comes back in the result.
func ValidateWith(schema any, schemaURI string, instance any, resolve Resolver) (result ValidationResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			sp, ok := r.(schemaPanic)
			if !ok {
				panic(r)
			}
			result, err = ValidationResult{}, sp.err
		}
	}()
	ctx := &validationCtx{resolve: resolve, seen: map[string]bool{}, patterns: map[string]*regexp.Regexp{}}
	errors := validateNode(schema, instance, "", schemaURI, ctx)
	return ValidationResult{Valid: len(errors) == 0, Errors: errors}, nil
}

func newError(instancePath, schemaPath, keyword, message string) ValidationError {
	return ValidationError{InstancePath: instancePath, SchemaPath: schemaPath, Keyword: keyword, Message: message}
}

func validateNode(schema, instance any, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	if b, ok := schema.(bool); ok {
		if b {
			return nil
		}
		return []ValidationError{newError(instancePath, schemaPath, "false", "no value is accepted here")}
	}
	node, ok := asObject(schema)
	if !ok {
		fail("%s: expected a schema object or boolean", schemaPath)
	}

	var errors []ValidationError

	// $ref first. A sibling keyword still applies in 2020-12, so this is not a jump.
	if ref, ok := node["$ref"].(string); ok {
		errors = append(errors, validateRef(ref, instance, instancePath, schemaPath, ctx)...)
	}

	// type is a gate: once it fails, every type-specific keyword below would
	// only repeat the same complaint with a different noun.
	if raw, present := node["type"]; present {
		wanted := []any{raw}
		if list, ok := raw.([]any); ok {
			wanted = list
		}
		matched := false
		names := make([]string, 0, len(wanted))
		for _, want := range wanted {
			name, ok := want.(string)
			if !ok {
				fail("%s/type: type members must be strings", schemaPath)
			}
			names = append(names, name)
			if matchesType(instance, name) {
				matched = true
			}
		}
		if !matched {
			return append(errors, newError(instancePath, schemaPath+"/type", "type",
				fmt.Sprintf("expected %s, got %s", strings.Join(names, ", "), jsonTypeOf(instance))))
		}
	}

	if want, present := node["const"]; present && canonical(instance) != canonical(want) {
		errors = append(errors, newError(instancePath, schemaPath+"/const", "const",
			fmt.Sprintf("expected %s, got %s", quoteValue(want), quoteValue(instance))))
	}

	if allowed, ok := node["enum"].([]any); ok {
		found := false
		rendered := make([]string, len(allowed))
		instanceForm := canonical(instance)
		for i, candidate := range allowed {
			rendered[i] = quoteValue(candidate)
			if canonical(candidate) == instanceForm {
				found = true
			}
		}
		if !found {
			errors = append(errors, newError(instancePath, schemaPath+"/enum", "enum",
				fmt.Sprintf("%s is not one of: %s", quoteValue(instance), strings.Join(rendered, ", "))))
		}
	}

	switch value := instance.(type) {
	case string:
		errors = append(errors, validateString(node, value, instancePath, schemaPath, ctx)...)
	case []any:
		errors = append(errors, validateArray(node, value, instancePath, schemaPath, ctx)...)
	case map[string]any:
		errors = append(errors, validateObject(node, value, instancePath, schemaPath, ctx)...)
	}
	if f, ok := asNumber(instance); ok {
		errors = append(errors, validateNumber(node, f, instancePath, schemaPath)...)
	}

	return append(errors, validateApplicators(node, instance, instancePath, schemaPath, ctx)...)
}

func validateRef(ref string, instance any, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	base, fragment := ref, ""
	if hash := strings.Index(ref, "#"); hash >= 0 {
		base, fragment = ref[:hash], ref[hash+1:]
	}
	targetURI := base
	if base == "" {
		targetURI, _, _ = strings.Cut(schemaPath, "#")
	}
	document, ok := ctx.resolve(targetURI)
	if !ok {
		fail("%s/$ref: unknown schema %q", schemaPath, targetURI)
	}
	target := pointerInto(document, fragment, targetURI)
	nextPath := targetURI + "#" + fragment
	cycleKey := nextPath + " " + instancePath
	if ctx.seen[cycleKey] {
		return nil
	}
	ctx.seen[cycleKey] = true
	defer delete(ctx.seen, cycleKey)
	return validateNode(target, instance, instancePath, nextPath, ctx)
}

func pointerInto(document any, fragment, uri string) any {
	if fragment == "" {
		return document
	}
	node := document
	for _, rawToken := range strings.Split(fragment, "/")[1:] {
		token := unescapePointer(rawToken)
		switch container := node.(type) {
		case map[string]any:
			next, ok := container[token]
			if !ok {
				fail("%s: cannot resolve pointer %s", uri, fragment)
			}
			node = next
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(container) {
				fail("%s: cannot resolve pointer %s", uri, fragment)
			}
			node = container[index]
		default:
			fail("%s: cannot resolve pointer %s", uri, fragment)
		}
	}
	return node
}

func validateString(schema map[string]any, value, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	var errors []ValidationError
	length := utf8.RuneCountInString(value)

	if limit, ok := schemaNumber(schema, "minLength"); ok && float64(length) < limit {
		errors = append(errors, newError(instancePath, schemaPath+"/minLength", "minLength",
			fmt.Sprintf("length %d is below the minimum of %s", length, formatNumber(limit))))
	}
	if limit, ok := schemaNumber(schema, "maxLength"); ok && float64(length) > limit {
		errors = append(errors, newError(instancePath, schemaPath+"/maxLength", "maxLength",
			fmt.Sprintf("length %d exceeds the maximum of %s", length, formatNumber(limit))))
	}
	if pattern, ok := schema["pattern"].(string); ok {
		if !ctx.regex(pattern, schemaPath+"/pattern").MatchString(value) {
			errors = append(errors, newError(instancePath, schemaPath+"/pattern", "pattern",
				fmt.Sprintf("%s does not match %s", quoteJSON(value), quoteJSON(pattern))))
		}
	}
	if format, ok := schema["format"].(string); ok {
		if format == "regex" {
			valid := !nonPortableRegex.MatchString(value)
			if valid {
				if _, err := regexp.Compile(value); err != nil {
					valid = false
				}
			}
			if !valid {
				errors = append(errors, newError(instancePath, schemaPath+"/format", "format",
					fmt.Sprintf("%s is not a portable regular expression", quoteJSON(value))))
			}
		} else {
			pattern := formatPatterns[format]
			if pattern == nil {
				fail("%s/format: unsupported format %q", schemaPath, format)
			}
			if !pattern.MatchString(value) {
				errors = append(errors, newError(instancePath, schemaPath+"/format", "format",
					fmt.Sprintf("%s is not a valid %s", quoteJSON(value), format)))
			}
		}
	}
	return errors
}

func validateNumber(schema map[string]any, value float64, instancePath, schemaPath string) []ValidationError {
	var errors []ValidationError
	bound := func(keyword string, fails func(limit float64) bool, text func(limit string) string) {
		if limit, ok := schemaNumber(schema, keyword); ok && fails(limit) {
			errors = append(errors, newError(instancePath, schemaPath+"/"+keyword, keyword, text(formatNumber(limit))))
		}
	}
	shown := formatNumber(value)
	bound("minimum", func(l float64) bool { return value < l },
		func(l string) string { return fmt.Sprintf("%s is below the minimum of %s", shown, l) })
	bound("maximum", func(l float64) bool { return value > l },
		func(l string) string { return fmt.Sprintf("%s exceeds the maximum of %s", shown, l) })
	bound("exclusiveMinimum", func(l float64) bool { return value <= l },
		func(l string) string { return fmt.Sprintf("%s must be greater than %s", shown, l) })
	bound("exclusiveMaximum", func(l float64) bool { return value >= l },
		func(l string) string { return fmt.Sprintf("%s must be less than %s", shown, l) })

	if step, ok := schemaNumber(schema, "multipleOf"); ok && step > 0 {
		ratio := value / step
		if math.Abs(ratio-math.Round(ratio)) > 1e-9 {
			errors = append(errors, newError(instancePath, schemaPath+"/multipleOf", "multipleOf",
				fmt.Sprintf("%s is not a multiple of %s", shown, formatNumber(step))))
		}
	}
	return errors
}

func validateArray(schema map[string]any, value []any, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	var errors []ValidationError

	if limit, ok := schemaNumber(schema, "minItems"); ok && float64(len(value)) < limit {
		errors = append(errors, newError(instancePath, schemaPath+"/minItems", "minItems",
			fmt.Sprintf("%d items is below the minimum of %s", len(value), formatNumber(limit))))
	}
	if limit, ok := schemaNumber(schema, "maxItems"); ok && float64(len(value)) > limit {
		errors = append(errors, newError(instancePath, schemaPath+"/maxItems", "maxItems",
			fmt.Sprintf("%d items exceeds the maximum of %s", len(value), formatNumber(limit))))
	}
	if unique, ok := schema["uniqueItems"].(bool); ok && unique {
		seen := map[string]int{}
		for i, item := range value {
			key := canonical(item)
			if first, duplicate := seen[key]; duplicate {
				errors = append(errors, newError(fmt.Sprintf("%s/%d", instancePath, i),
					schemaPath+"/uniqueItems", "uniqueItems",
					fmt.Sprintf("duplicates the item at index %d", first)))
				continue
			}
			seen[key] = i
		}
	}
	if items, present := schema["items"]; present {
		for i, item := range value {
			errors = append(errors, validateNode(items, item,
				fmt.Sprintf("%s/%d", instancePath, i), schemaPath+"/items", ctx)...)
		}
	}
	return errors
}

func validateObject(schema map[string]any, value map[string]any, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	var errors []ValidationError
	present := slices.Sorted(maps.Keys(value))

	if required, ok := schema["required"].([]any); ok {
		for _, raw := range required {
			name, ok := raw.(string)
			if !ok {
				fail("%s/required: members must be strings", schemaPath)
			}
			if _, found := value[name]; !found {
				errors = append(errors, newError(instancePath, schemaPath+"/required", "required",
					fmt.Sprintf("missing required property %q", name)))
			}
		}
	}

	if dependents, ok := asObject(schema["dependentRequired"]); ok {
		for _, trigger := range slices.Sorted(maps.Keys(dependents)) {
			if _, found := value[trigger]; !found {
				continue
			}
			needed, ok := dependents[trigger].([]any)
			if !ok {
				continue
			}
			for _, raw := range needed {
				name, ok := raw.(string)
				if !ok {
					continue
				}
				if _, found := value[name]; !found {
					errors = append(errors, newError(instancePath,
						schemaPath+"/dependentRequired/"+escapePointer(trigger), "dependentRequired",
						fmt.Sprintf("property %q is required when %q is present", name, trigger)))
				}
			}
		}
	}

	if limit, ok := schemaNumber(schema, "minProperties"); ok && float64(len(present)) < limit {
		errors = append(errors, newError(instancePath, schemaPath+"/minProperties", "minProperties",
			fmt.Sprintf("%d properties is below the minimum of %s", len(present), formatNumber(limit))))
	}
	if limit, ok := schemaNumber(schema, "maxProperties"); ok && float64(len(present)) > limit {
		errors = append(errors, newError(instancePath, schemaPath+"/maxProperties", "maxProperties",
			fmt.Sprintf("%d properties exceeds the maximum of %s", len(present), formatNumber(limit))))
	}

	properties, _ := asObject(schema["properties"])
	patternProperties, _ := asObject(schema["patternProperties"])

	if names, present := schema["propertyNames"]; present {
		for _, name := range slices.Sorted(maps.Keys(value)) {
			errors = append(errors, validateNode(names, name,
				instancePath+"/"+escapePointer(name), schemaPath+"/propertyNames", ctx)...)
		}
	}

	additional, hasAdditional := schema["additionalProperties"]
	for _, name := range present {
		matched := false
		if sub, ok := properties[name]; ok {
			matched = true
			errors = append(errors, validateNode(sub, value[name],
				instancePath+"/"+escapePointer(name),
				schemaPath+"/properties/"+escapePointer(name), ctx)...)
		}
		for _, pattern := range slices.Sorted(maps.Keys(patternProperties)) {
			if ctx.regex(pattern, schemaPath+"/patternProperties").MatchString(name) {
				matched = true
				errors = append(errors, validateNode(patternProperties[pattern], value[name],
					instancePath+"/"+escapePointer(name),
					schemaPath+"/patternProperties/"+escapePointer(pattern), ctx)...)
			}
		}
		if matched || !hasAdditional {
			continue
		}
		if allowed, ok := additional.(bool); ok && !allowed {
			errors = append(errors, newError(instancePath+"/"+escapePointer(name),
				schemaPath+"/additionalProperties", "additionalProperties",
				fmt.Sprintf("property %q is not allowed here", name)))
			continue
		}
		errors = append(errors, validateNode(additional, value[name],
			instancePath+"/"+escapePointer(name), schemaPath+"/additionalProperties", ctx)...)
	}
	return errors
}

func validateApplicators(schema map[string]any, instance any, instancePath, schemaPath string, ctx *validationCtx) []ValidationError {
	var errors []ValidationError

	if branches, ok := schema["allOf"].([]any); ok {
		for i, branch := range branches {
			errors = append(errors, validateNode(branch, instance, instancePath,
				fmt.Sprintf("%s/allOf/%d", schemaPath, i), ctx)...)
		}
	}

	if branches, ok := schema["anyOf"].([]any); ok {
		failures, matched := branchResults(branches, instance, instancePath, schemaPath, "anyOf", ctx)
		if matched == 0 {
			errors = append(errors, newError(instancePath, schemaPath+"/anyOf", "anyOf",
				fmt.Sprintf("does not match any of the %d allowed shapes: %s", len(branches), summarise(failures))))
		}
	}

	if branches, ok := schema["oneOf"].([]any); ok {
		failures, matched := branchResults(branches, instance, instancePath, schemaPath, "oneOf", ctx)
		switch {
		case matched == 0:
			errors = append(errors, newError(instancePath, schemaPath+"/oneOf", "oneOf",
				fmt.Sprintf("does not match any of the %d allowed shapes: %s", len(branches), summarise(failures))))
		case matched > 1:
			errors = append(errors, newError(instancePath, schemaPath+"/oneOf", "oneOf",
				fmt.Sprintf("matches %d of the %d allowed shapes, expected exactly one", matched, len(branches))))
		}
	}

	if sub, present := schema["not"]; present {
		if len(validateNode(sub, instance, instancePath, schemaPath+"/not", ctx)) == 0 {
			errors = append(errors, newError(instancePath, schemaPath+"/not", "not",
				"matches a shape that is not allowed here"))
		}
	}

	if condition, present := schema["if"]; present {
		branch := "else"
		if len(validateNode(condition, instance, instancePath, schemaPath+"/if", ctx)) == 0 {
			branch = "then"
		}
		if sub, present := schema[branch]; present {
			errors = append(errors, validateNode(sub, instance, instancePath, schemaPath+"/"+branch, ctx)...)
		}
	}

	return errors
}

func branchResults(branches []any, instance any, instancePath, schemaPath, keyword string, ctx *validationCtx) ([][]ValidationError, int) {
	failures := make([][]ValidationError, len(branches))
	matched := 0
	for i, branch := range branches {
		failures[i] = validateNode(branch, instance, instancePath,
			fmt.Sprintf("%s/%s/%d", schemaPath, keyword, i), ctx)
		if len(failures[i]) == 0 {
			matched++
		}
	}
	return failures, matched
}

// summarise keeps one line per branch, so a oneOf failure still names the field
// that was wrong instead of only saying "no match".
func summarise(failures [][]ValidationError) string {
	parts := make([]string, len(failures))
	for i, branch := range failures {
		detail := "ok"
		if len(branch) > 0 {
			path := branch[0].InstancePath
			if path == "" {
				path = "/"
			}
			detail = path + ": " + branch[0].Message
		}
		parts[i] = fmt.Sprintf("[%d] %s", i, detail)
	}
	return strings.Join(parts, "; ")
}
