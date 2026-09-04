package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spec is only worth having if it cannot drift from the router.
//
// This walks the Go sources for every route that is actually mounted and
// compares that set against docs/api/openapi.yaml, in both directions: a route
// added without an entry fails, and an entry left behind after a route is
// removed fails too. It is static — no database, no server — so it runs in
// `go test ./...` on a machine with nothing installed.

const specPath = "../../../docs/api/openapi.yaml"

// mux.Handle("GET /api/v1/runs", …) and the same on the WebSocket mux.
var mountedRouteRe = regexp.MustCompile(`\b(?:mux|root)\.Handle\("([A-Z]+) (/[^"]*)"`)

type openAPISpec struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	WebSockets map[string]yaml.Node            `yaml:"x-websockets"`
}

func TestOpenAPICoversEveryMountedRoute(t *testing.T) {
	mounted := mountedRoutes(t)
	documented := documentedRoutes(t)

	for _, route := range mounted {
		if !documented[route] {
			t.Errorf("%s is mounted by the router but not in docs/api/openapi.yaml", route)
		}
	}
	for route := range documented {
		if !contains(mounted, route) {
			t.Errorf("%s is in docs/api/openapi.yaml but no route mounts it", route)
		}
	}

	// A guard on the guard: if the source scan stops finding routes, the
	// comparison above passes vacuously and the spec is unchecked from then on.
	if len(mounted) < 20 {
		t.Fatalf("only found %d mounted routes, the source scan is probably broken: %v", len(mounted), mounted)
	}
}

// The spec has to parse and describe a response for every operation, or it
// documents a shape nobody can generate a client from.
func TestOpenAPIOperationsDescribeTheirResponses(t *testing.T) {
	spec := loadSpec(t)

	for path, operations := range spec.Paths {
		for method, node := range operations {
			var operation struct {
				Summary   string               `yaml:"summary"`
				Responses map[string]yaml.Node `yaml:"responses"`
			}
			if err := node.Decode(&operation); err != nil {
				t.Fatalf("%s %s: %v", strings.ToUpper(method), path, err)
			}
			if operation.Summary == "" {
				t.Errorf("%s %s has no summary", strings.ToUpper(method), path)
			}
			if len(operation.Responses) == 0 {
				t.Errorf("%s %s documents no responses", strings.ToUpper(method), path)
			}
			// Every tenant-scoped route can fail the membership check, and a
			// client that does not handle that renders an empty page instead
			// of "switch organization".
			if strings.HasPrefix(path, "/api/v1/") && requiresOrg(path) {
				if _, ok := operation.Responses["403"]; !ok {
					t.Errorf("%s %s is org-scoped but documents no 403", strings.ToUpper(method), path)
				}
			}
		}
	}
}

// requiresOrg reports whether a documented path is one of the few that
// deliberately are NOT org-scoped: signing up, logging in, accepting an invite
// and redeeming a pairing code all happen before the caller has an
// organization to name.
func requiresOrg(path string) bool {
	switch path {
	case "/healthz", "/readyz",
		"/api/v1/auth/signup", "/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/me",
		"/api/v1/orgs", "/api/v1/invites/accept", "/api/v1/runtimes/redeem",
		"/api/v1/runs/{runID}/artifacts/presign":
		return false
	default:
		return true
	}
}

// mountedRoutes scans the Go sources for every route the router mounts.
//
// Reading the source rather than asking the mux is deliberate: net/http's
// ServeMux does not expose its patterns, and a hand-written list next to the
// test would be one more thing that drifts.
func mountedRoutes(t *testing.T) []string {
	t.Helper()

	roots := []string{".", "../../internal"}
	var routes []string

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, err := os.ReadFile(path) //nolint:gosec // a path this test walked itself
			if err != nil {
				return err
			}
			for _, match := range mountedRouteRe.FindAllStringSubmatch(string(source), -1) {
				routes = append(routes, match[1]+" "+match[2])
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s for routes: %v", root, err)
		}
	}

	sort.Strings(routes)
	return routes
}

// documentedRoutes flattens the spec into the same "METHOD /path" keys.
func documentedRoutes(t *testing.T) map[string]bool {
	t.Helper()

	spec := loadSpec(t)
	routes := map[string]bool{}
	for path, operations := range spec.Paths {
		for method := range operations {
			routes[strings.ToUpper(method)+" "+path] = true
		}
	}
	// The two sockets are keyed by the same "METHOD /path" string, because
	// OpenAPI 3.1 has no way to describe a WebSocket and leaving them out would
	// make the control plane the one part of the API with no spec.
	for route := range spec.WebSockets {
		routes[route] = true
	}
	return routes
}

func loadSpec(t *testing.T) openAPISpec {
	t.Helper()

	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var spec openAPISpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	if len(spec.Paths) == 0 {
		t.Fatalf("%s documents no paths", specPath)
	}
	return spec
}

func contains(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}
