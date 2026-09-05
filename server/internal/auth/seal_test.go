package auth_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
)

// This file is the executable half of ADR-007.
//
// The ADR claims that a handler cannot build an auth.OrgScope out of a path
// segment, a body or a query string. That claim is worth nothing as prose: it
// has to be a property of the type. It is, and these tests are what keeps it
// one.
//
// Two things together make it true, and each has a test below:
//
//  1. Every field of Caller, OrgScope and RuntimeCaller is unexported, so the
//     only value of those types another package can write down is the zero
//     value — which names no user, no organization and no runtime.
//  2. No exported function in package auth builds one of those types out of
//     its arguments. The exported functions that return them either read one
//     back out of a context (CallerFrom, OrgScopeFrom, MustOrgScope,
//     RuntimeCallerFrom, MustRuntimeCaller), project one that already exists
//     (OrgScope.Caller), or verify a credential against the database first
//     (Sessions.Authenticate, AuthenticateRuntime).
//
// This file lives in package auth_test on purpose: it can only reach what any
// other package can reach.

// The line below is what the ADR says must not compile. It is kept as a
// comment rather than a build-tagged file so that it sits next to the test
// that explains it:
//
//	scope := auth.OrgScope{OrgID: idFromPath}   // unknown field OrgID
//
// TestAuthPrincipalsAreSealed is the mechanical version of that claim.
func TestAuthPrincipalsAreSealed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
	}{
		{"Caller", auth.Caller{}},
		{"OrgScope", auth.OrgScope{}},
		{"RuntimeCaller", auth.RuntimeCaller{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			typ := reflect.TypeOf(tt.value)
			if typ.NumField() == 0 {
				t.Fatalf("auth.%s has no fields at all; this test is no longer checking anything", tt.name)
			}
			for i := range typ.NumField() {
				field := typ.Field(i)
				if field.IsExported() {
					t.Errorf("auth.%s.%s is exported: any package can now build a %s "+
						"for an organization the caller never proved membership in (ADR-007)",
						tt.name, field.Name, tt.name)
				}
			}
		})
	}
}

// TestZeroPrincipalsNameNobody pins the consequence: the one value an outside
// package CAN construct is inert, and the Must* accessors refuse it.
func TestZeroPrincipalsNameNobody(t *testing.T) {
	var scope auth.OrgScope // the most a package outside auth can build
	if scope.OrgID() != uuid.Nil || scope.UserID() != uuid.Nil || scope.Role() != auth.Role("") {
		t.Errorf("a zero OrgScope names something: org=%v user=%v role=%q",
			scope.OrgID(), scope.UserID(), scope.Role())
	}

	var rc auth.RuntimeCaller
	if rc.OrgID() != uuid.Nil || rc.RuntimeID() != uuid.Nil || rc.TokenID() != uuid.Nil {
		t.Errorf("a zero RuntimeCaller names something: %v %v %v", rc.OrgID(), rc.RuntimeID(), rc.TokenID())
	}

	// A context that never went through the middleware yields nothing, and the
	// Must* helpers fail closed rather than handing back the zero value.
	if _, err := auth.MustOrgScope(context.Background()); err == nil {
		t.Error("MustOrgScope accepted a context with no scope in it")
	}
	if _, err := auth.MustRuntimeCaller(context.Background()); err == nil {
		t.Error("MustRuntimeCaller accepted a context with no runtime caller in it")
	}
}

// derivedFromContextOrCredential lists every exported function or method in
// package auth that is allowed to hand back a principal, and why.
//
// Adding a name here is the moment to ask whether the new function can be
// reached from a handler with a value the handler chose. If it can, ADR-007 is
// no longer true and the ADR has to change with it.
var derivedFromContextOrCredential = map[string]string{
	"CallerFrom":            "reads a Caller back out of the request context",
	"OrgScopeFrom":          "reads an OrgScope back out of the request context",
	"MustOrgScope":          "reads an OrgScope back out of the request context, failing closed",
	"RuntimeCallerFrom":     "reads a RuntimeCaller back out of the request context",
	"MustRuntimeCaller":     "reads a RuntimeCaller back out of the request context, failing closed",
	"OrgScope.Caller":       "projects the Caller already inside a scope",
	"Sessions.Authenticate": "resolves a live session row to the user behind it",
	"AuthenticateRuntime":   "resolves a runtime token row to the daemon behind it",
}

// TestNoExportedConstructorForAPrincipal parses this package's own source and
// fails if some new exported function returns a Caller, an OrgScope or a
// RuntimeCaller without being on the list above.
func TestNoExportedConstructorForAPrincipal(t *testing.T) {
	principals := map[string]bool{"Caller": true, "OrgScope": true, "RuntimeCaller": true}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package auth: %v", err)
	}

	var unexpected []string
	seen := map[string]bool{}
	sources := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		sources++
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			if !returnsAnyOf(fn.Type.Results, principals) {
				continue
			}
			qualified := qualifiedName(fn)
			seen[qualified] = true
			if _, allowed := derivedFromContextOrCredential[qualified]; !allowed {
				unexpected = append(unexpected, qualified+" at "+fset.Position(fn.Pos()).String())
			}
		}
	}
	if sources == 0 {
		t.Fatal("parsed no source file of package auth; this test is checking nothing")
	}

	sort.Strings(unexpected)
	for _, name := range unexpected {
		t.Errorf("exported %s returns an auth principal but is not a documented "+
			"context or credential lookup: either make it unexported, or add it to "+
			"derivedFromContextOrCredential and update ADR-007 to match", name)
	}

	// The other direction: an allowlist entry that no longer names anything is
	// a licence nobody is using, and it will quietly bless the next function
	// that happens to take the name.
	for name := range derivedFromContextOrCredential {
		if !seen[name] {
			t.Errorf("derivedFromContextOrCredential lists %s, which no longer exists; remove it", name)
		}
	}
}

// returnsAnyOf reports whether any result of the signature is one of the named
// types, by value or by pointer.
func returnsAnyOf(results *ast.FieldList, names map[string]bool) bool {
	for _, result := range results.List {
		expr := result.Type
		if star, isPointer := expr.(*ast.StarExpr); isPointer {
			expr = star.X
		}
		ident, isIdent := expr.(*ast.Ident)
		if isIdent && names[ident.Name] {
			return true
		}
	}
	return false
}

// qualifiedName renders a method as "Receiver.Method" and a function as
// "Function", matching the keys of derivedFromContextOrCredential.
func qualifiedName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	expr := fn.Recv.List[0].Type
	if star, isPointer := expr.(*ast.StarExpr); isPointer {
		expr = star.X
	}
	if ident, isIdent := expr.(*ast.Ident); isIdent {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// TestNoPackageOutsideAuthWritesAPrincipalLiteral is belt to the compiler's
// braces. The compiler already rejects a literal with a field in it; what this
// catches is the shape that still compiles — auth.OrgScope{} — appearing
// somewhere that then treats it as a real scope.
//
// The only uses that survive are the zero values returned alongside an error.
func TestNoPackageOutsideAuthWritesAPrincipalLiteral(t *testing.T) {
	const serverRoot = "../.."

	fset := token.NewFileSet()
	// The scan is only worth anything if it is looking at the right tree, and
	// a walk that finds nothing passes silently. These zero literals — the
	// `return auth.OrgScope{}, uuid.Nil, err` lines in the run and project
	// handlers — are what proves it walked.
	inspected := 0
	err := filepath.WalkDir(serverRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/internal/auth/") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, isLit := n.(*ast.CompositeLit)
			if !isLit {
				return true
			}
			sel, isSel := lit.Type.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			pkgIdent, isIdent := sel.X.(*ast.Ident)
			if !isIdent || pkgIdent.Name != "auth" {
				return true
			}
			switch sel.Sel.Name {
			case "Caller", "OrgScope", "RuntimeCaller":
			default:
				return true
			}
			inspected++
			if len(lit.Elts) != 0 {
				// Unreachable through the compiler today; this reports it as
				// the tenancy bug it is rather than as a build failure.
				t.Errorf("%s: builds an auth.%s with fields set; a principal may only "+
					"come from the auth middleware (ADR-007)",
					fset.Position(lit.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", serverRoot, err)
	}
	if inspected == 0 {
		t.Fatalf("found no auth principal literal anywhere under %s; the walk is not "+
			"reaching the packages it is meant to police", serverRoot)
	}
}
