package project_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// SystemGet and SystemApplicationMap take an org id as a plain argument
// instead of an auth.OrgScope, because the scheduler that calls them runs
// without a request and therefore without a scope (ADR-007). That is the one
// legitimate provenance for an org id that auth did not resolve, so it needs a
// gate: a handler reaching for one of these methods would be reading a tenant
// out of a path again, which is exactly what sealing auth.OrgScope closed.
//
// systemReadCallers is that gate. A file is listed with the reason it is
// allowed to originate work for an organization nobody is signed in to.
var systemReadCallers = map[string]string{
	"internal/run/assign.go": "the scheduler builds a run.assign frame for a run it has already claimed; " +
		"the org id is the one on the claimed row",
}

var systemReadMethods = map[string]bool{
	"SystemGet":            true,
	"SystemApplicationMap": true,
}

func TestSystemProjectReadsHaveNoRequestCallers(t *testing.T) {
	const serverRoot = "../.."

	fset := token.NewFileSet()
	used := map[string]bool{}
	var violations []string

	err := filepath.WalkDir(serverRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(serverRoot, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		// The definitions themselves live here.
		if strings.HasPrefix(rel, "internal/project/") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel || !systemReadMethods[sel.Sel.Name] {
				return true
			}
			if _, allowed := systemReadCallers[rel]; allowed {
				used[rel] = true
				return true
			}
			violations = append(violations, fset.Position(call.Pos()).String()+": "+sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", serverRoot, err)
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("%s is called outside the scheduler. Anything serving a request has an "+
			"auth.OrgScope and must use the scope-taking method; an org id chosen by the "+
			"caller is the leak ADR-007 closed. If this really is server-originated work, "+
			"add the file to systemReadCallers with the reason.", v)
	}

	for file := range systemReadCallers {
		if !used[file] {
			t.Errorf("systemReadCallers lists %s, which no longer calls a System* read; remove it "+
				"so the allowlist cannot bless an unrelated future caller", file)
		}
	}
}
