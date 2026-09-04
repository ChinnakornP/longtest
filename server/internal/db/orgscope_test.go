package db

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file is the primary cross-tenant leak gate of the MVP.
//
// Every query that touches a domain table must bind org_id to a query
// parameter. A query that filters only by a primary key would happily return
// another organization's row, and no amount of care in the service layer is a
// substitute for the guarantee that the SQL itself cannot express that.
//
// The check is static text analysis of internal/db/queries/*.sql, so it runs in
// `go test ./...` with no database. Adding a query that breaks the rule fails
// the build.

// domainTables carries customer data and is subject to the org_id rule.
// This list is the contract from LONG-5 plus finding_evidence, which was added
// with the same obligations.
var domainTables = map[string]bool{
	"projects":             true,
	"runtimes":             true,
	"runs":                 true,
	"run_events":           true,
	"pages":                true,
	"elements":             true,
	"workflows":            true,
	"test_cases":           true,
	"test_case_versions":   true,
	"executions":           true,
	"execution_steps":      true,
	"execution_assertions": true,
	"artifacts":            true,
	"findings":             true,
	"finding_evidence":     true,
}

// tenancyTables ARE the tenancy layer: they are how an org_id is established in
// the first place, so they cannot take one as an input. They are listed
// explicitly rather than simply omitted so that a new table cannot slip into
// neither list unnoticed (TestEveryTableIsClassified).
var tenancyTables = map[string]bool{
	"organizations":  true,
	"users":          true,
	"memberships":    true,
	"sessions":       true,
	"runtime_tokens": true,
	"pairing_codes":  true,
}

const queriesDir = "queries"

var (
	queryNameRe = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)\s*:(\w+)\s*$`)
	// FROM / JOIN / INTO / UPDATE / DELETE FROM <table> [AS] [alias]
	tableRefRe = regexp.MustCompile(`(?is)\b(?:from|join|insert\s+into|into|update)\s+"?([a-z_][a-z0-9_]*)"?(?:\s+(?:as\s+)?"?([a-z_][a-z0-9_]*)"?)?`)
	// Any mention of org_id, with the alias that qualifies it (if any).
	orgIDRefRe = regexp.MustCompile(`(?i)\b(?:([a-z_][a-z0-9_]*)\.)?org_id\b`)
	// org_id bound to a real query parameter, not to another column.
	orgIDBoundRe = regexp.MustCompile(`(?i)\b(?:[a-z_][a-z0-9_]*\.)?org_id\s*=\s*(?:\$\d+|sqlc\.n?arg\s*\()`)
	// INSERT INTO <table> ( ... org_id ... )
	insertColumnsRe = regexp.MustCompile(`(?is)insert\s+into\s+"?([a-z_][a-z0-9_]*)"?\s*\(([^)]*)\)`)
	exemptRe        = regexp.MustCompile(`(?im)^--\s*org-scope-exempt:\s*(\S.*)$`)
	// Reserved words that follow a table name and are not an alias.
	notAnAlias = map[string]bool{
		"as": true, "on": true, "where": true, "set": true, "values": true,
		"select": true, "using": true, "join": true, "inner": true, "left": true,
		"right": true, "full": true, "cross": true, "order": true, "group": true,
		"limit": true, "offset": true, "returning": true, "and": true, "or": true,
		"for": true, "conflict": true, "do": true, "nothing": true, "update": true,
	}
)

type namedQuery struct {
	Name    string
	Command string
	File    string
	Body    string
}

// parseQueries splits every .sql file in dir into its named queries.
func parseQueries(t *testing.T, dir string) []namedQuery {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var out []namedQuery
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path) //nolint:gosec // fixed, test-local path
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		content := string(raw)

		locs := queryNameRe.FindAllStringSubmatchIndex(content, -1)
		for i, loc := range locs {
			end := len(content)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			out = append(out, namedQuery{
				Name:    content[loc[2]:loc[3]],
				Command: content[loc[4]:loc[5]],
				File:    e.Name(),
				Body:    content[loc[1]:end],
			})
		}
	}

	if len(out) == 0 {
		t.Fatalf("no queries found in %s - the lint would pass vacuously", dir)
	}
	return out
}

// stripComments removes line comments so that a table name mentioned in prose
// is not mistaken for a table reference.
func stripComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// relation is one table reference in a statement.
type relation struct {
	Table string
	Alias string // empty when the reference is unaliased
}

func relations(sql string) []relation {
	var out []relation
	for _, m := range tableRefRe.FindAllStringSubmatch(sql, -1) {
		table, alias := strings.ToLower(m[1]), strings.ToLower(m[2])
		if notAnAlias[alias] {
			alias = ""
		}
		out = append(out, relation{Table: table, Alias: alias})
	}
	return out
}

// checkOrgScope reports why a query violates the org_id rule, or nil.
//
// Split out from the test loop so TestOrgScopeLintCatchesViolations can feed it
// known-bad SQL: a lint that only ever sees passing input is indistinguishable
// from one that always passes.
func checkOrgScope(q namedQuery) error {
	if reason := exemptRe.FindStringSubmatch(q.Body); reason != nil {
		// An exemption is allowed but must say why, in the query, where a
		// reviewer of that query will see it.
		if len(strings.TrimSpace(reason[1])) < 20 {
			return fmt.Errorf("org-scope-exempt needs a real justification, got %q", reason[1])
		}
		return nil
	}

	sql := stripComments(q.Body)
	rels := relations(sql)

	var domain []relation
	for _, r := range rels {
		if domainTables[r.Table] {
			domain = append(domain, r)
		}
	}
	if len(domain) == 0 {
		return nil // touches only tenancy tables
	}

	// Which org_id references exist, and how are they qualified?
	bareOrgID := false
	qualified := map[string]bool{}
	for _, m := range orgIDRefRe.FindAllStringSubmatch(sql, -1) {
		if m[1] == "" {
			bareOrgID = true
		} else {
			qualified[strings.ToLower(m[1])] = true
		}
	}

	// Columns supplied by an INSERT, keyed by target table.
	inserted := map[string]bool{}
	for _, m := range insertColumnsRe.FindAllStringSubmatch(sql, -1) {
		for _, col := range strings.Split(m[2], ",") {
			if strings.EqualFold(strings.TrimSpace(col), "org_id") {
				inserted[strings.ToLower(m[1])] = true
			}
		}
	}

	// 1. org_id must be bound to a query parameter somewhere, or be an
	//    explicitly supplied INSERT column.
	if !orgIDBoundRe.MatchString(sql) && len(inserted) == 0 {
		return fmt.Errorf("query touches domain table(s) %s but never binds org_id to a parameter; "+
			"either scope it or annotate it with `-- org-scope-exempt: <reason>`",
			tableNames(domain))
	}

	// 2. Every domain relation in the statement must have its own org_id
	//    constrained - a join that forgets one leaks through it.
	for _, r := range domain {
		if r.Alias != "" {
			if !qualified[r.Alias] {
				return fmt.Errorf("domain table %q (alias %q) is referenced without any %s.org_id predicate",
					r.Table, r.Alias, r.Alias)
			}
			continue
		}
		if inserted[r.Table] || bareOrgID || qualified[r.Table] {
			continue
		}
		return fmt.Errorf("domain table %q is referenced without an org_id predicate", r.Table)
	}
	return nil
}

// TestQueriesAreOrgScoped is the cross-tenant leak gate.
func TestQueriesAreOrgScoped(t *testing.T) {
	for _, q := range parseQueries(t, queriesDir) {
		t.Run(q.File+"/"+q.Name, func(t *testing.T) {
			if err := checkOrgScope(q); err != nil {
				t.Fatalf("%v\n%s", err, q.Body)
			}
		})
	}
}

// TestOrgScopeLintCatchesViolations feeds the checker queries that must be
// rejected. Without this the lint above could be silently broken by a bad
// regex and would keep reporting success.
func TestOrgScopeLintCatchesViolations(t *testing.T) {
	bad := []struct {
		name string
		sql  string
	}{
		{
			name: "select by primary key only",
			sql:  "\nSELECT * FROM projects WHERE id = $1;\n",
		},
		{
			name: "update without org scope",
			sql:  "\nUPDATE runs SET status = 'cancelled' WHERE id = $1;\n",
		},
		{
			name: "delete without org scope",
			sql:  "\nDELETE FROM artifacts WHERE id = $1;\n",
		},
		{
			name: "insert that omits org_id",
			sql:  "\nINSERT INTO pages (project_id, ref, path) VALUES ($1, $2, $3) RETURNING *;\n",
		},
		{
			name: "join whose second domain table is unscoped",
			sql: "\nSELECT e.* FROM executions e JOIN test_cases tc ON tc.id = e.test_case_id\n" +
				"WHERE e.org_id = $1;\n",
		},
		{
			name: "org_id compared to a column instead of a parameter",
			sql: "\nSELECT e.* FROM executions e JOIN runs r ON r.id = e.run_id AND r.org_id = e.org_id\n" +
				"WHERE e.id = $1;\n",
		},
		{
			name: "exemption without a justification",
			sql:  "\n-- org-scope-exempt: because\nSELECT * FROM runs WHERE id = $1;\n",
		},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkOrgScope(namedQuery{Name: tt.name, File: "synthetic.sql", Body: tt.sql}); err == nil {
				t.Fatalf("expected the org-scope lint to reject:\n%s", tt.sql)
			}
		})
	}
}

// TestOrgScopeLintAcceptsValidShapes guards the other direction: the checker
// must not reject the query shapes this schema legitimately uses.
func TestOrgScopeLintAcceptsValidShapes(t *testing.T) {
	good := []struct {
		name string
		sql  string
	}{
		{"scoped select", "\nSELECT * FROM projects WHERE org_id = $1 AND id = $2;\n"},
		{"insert with org_id", "\nINSERT INTO pages (org_id, project_id, ref, path) VALUES ($1,$2,$3,$4);\n"},
		{
			"join correlated on org_id",
			"\nSELECT e.* FROM executions e JOIN test_cases tc ON tc.id = e.test_case_id AND tc.org_id = e.org_id\n" +
				"WHERE e.org_id = $1;\n",
		},
		{"tenancy table only", "\nSELECT * FROM users WHERE email = $1;\n"},
		{
			"justified exemption",
			"\n-- org-scope-exempt: platform maintenance sweeper, never reachable from a handler\n" +
				"UPDATE runs SET status = 'error' WHERE heartbeat_at < now();\n",
		},
	}

	for _, tt := range good {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkOrgScope(namedQuery{Name: tt.name, File: "synthetic.sql", Body: tt.sql}); err != nil {
				t.Fatalf("lint rejected a valid query: %v\n%s", err, tt.sql)
			}
		})
	}
}

// TestEveryTableIsClassified fails when a migration adds a table that neither
// list knows about, so a new table cannot quietly escape the org_id rule.
func TestEveryTableIsClassified(t *testing.T) {
	created := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)

	entries, err := os.ReadDir("../../migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	var unknown []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("../../migrations", e.Name())) //nolint:gosec // fixed, test-local path
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range created.FindAllStringSubmatch(string(raw), -1) {
			table := strings.ToLower(m[1])
			if !domainTables[table] && !tenancyTables[table] {
				unknown = append(unknown, fmt.Sprintf("%s (%s)", table, e.Name()))
			}
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Fatalf("these tables are in neither domainTables nor tenancyTables: %s\n"+
			"add them to the right list in orgscope_test.go so the org_id rule applies",
			strings.Join(unknown, ", "))
	}
}

func tableNames(rels []relation) string {
	seen := map[string]bool{}
	var names []string
	for _, r := range rels {
		if !seen[r.Table] {
			seen[r.Table] = true
			names = append(names, r.Table)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
