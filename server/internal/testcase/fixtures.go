package testcase

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// The fixture registry: names, and only names.
//
// A fixture is a starting state a run can establish — "signed in as an admin",
// "with a seeded employee". The planner is told the names so it can write
// `preconditions: ["fixture:logged_in_as_admin"]`, and it is told nothing else,
// because there is nothing else here to tell it. The values live in the
// daemon's sealed store on the operator's machine (daemon/security/fixtures.go)
// under a key this backend never holds.
//
// That split is what makes the precondition rule enforceable. Without a
// registry the server can only check a precondition's shape, and a planner
// talked into `fixture:logged_in_as_root` by a page would have that stored as a
// real case, waiting for a reviewer who has no way to know it is fiction.

// fixtureName mirrors the Precondition pattern in test-case.schema.json and
// the CHECK on project_fixtures.name. Three copies of one rule, in the three
// places that would each be a hole on their own: the contract the model writes
// against, the column, and this validation, which is what turns a violation
// into a 422 with a field name instead of a constraint error in a log.
var fixtureName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// FixtureView is a registered fixture as the API renders it.
type FixtureView struct {
	Name string `json:"name"`
	// Reference is the exact string a test case must use, spelled out so
	// nobody has to remember to prepend the prefix.
	Reference   string    `json:"reference"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// NewFixtureView renders a fixture row.
func NewFixtureView(f dbgen.ProjectFixture) FixtureView {
	return FixtureView{
		Name:        f.Name,
		Reference:   "fixture:" + f.Name,
		Description: f.Description,
		CreatedAt:   f.CreatedAt.Time.UTC(),
	}
}

// FixtureListResponse is the body of the fixture list.
type FixtureListResponse struct {
	Fixtures []FixtureView `json:"fixtures"`
}

// ListFixtures returns a project's registered fixture names.
func (s *Service) ListFixtures(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID) ([]dbgen.ProjectFixture, error) {
	fixtures, err := s.store.ListProjectFixtures(ctx, dbgen.ListProjectFixturesParams{
		OrgID: scope.OrgID, ProjectID: projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("list project fixtures: %w", db.Classify(err))
	}
	return fixtures, nil
}

// maxFixtureDescription matches the column's CHECK.
const maxFixtureDescription = 500

// RegisterFixture adds or re-describes a fixture name.
//
// Registering the same name twice succeeds and updates the description: this
// is a declaration of what exists, and a declaration that cannot be repeated
// is one an operator cannot run twice from a script.
func (s *Service) RegisterFixture(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID, name, description string) (dbgen.ProjectFixture, error) {
	if !fixtureName.MatchString(name) {
		return dbgen.ProjectFixture{}, httpx.InvalidField("name",
			"must be 1-64 characters of a-z, 0-9 and _, starting with a letter")
	}
	if len(description) > maxFixtureDescription {
		return dbgen.ProjectFixture{}, httpx.InvalidField("description",
			fmt.Sprintf("must be at most %d characters", maxFixtureDescription))
	}

	fixture, err := s.store.UpsertProjectFixture(ctx, dbgen.UpsertProjectFixtureParams{
		OrgID: scope.OrgID, ProjectID: projectID, Name: name, Description: description,
	})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrConflict) {
			// The composite FK to projects: the project is not this tenant's,
			// or does not exist. Reported as a 404 for the same reason every
			// other cross-tenant read is.
			return dbgen.ProjectFixture{}, httpx.NotFound("project not found")
		}
		return dbgen.ProjectFixture{}, fmt.Errorf("register fixture: %w", db.Classify(err))
	}
	return fixture, nil
}

// DeleteFixture removes a name from the registry.
//
// Test cases that reference it are deliberately left alone. Deleting them
// would throw away a reviewer's work over a registry edit, and leaving them is
// visible anyway: the next plan ingest rejects any case naming a fixture that
// is no longer registered, and an execute run's precondition fails loudly.
func (s *Service) DeleteFixture(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID, name string) error {
	deleted, err := s.store.DeleteProjectFixture(ctx, dbgen.DeleteProjectFixtureParams{
		OrgID: scope.OrgID, ProjectID: projectID, Name: name,
	})
	if err != nil {
		return fmt.Errorf("delete fixture: %w", db.Classify(err))
	}
	if deleted == 0 {
		return httpx.NotFound("no fixture named %q is registered for this project", name)
	}
	return nil
}
