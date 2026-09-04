package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/auth"
	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
	"github.com/ChinnakornP/longtest/server/pkg/qaschema"
)

// Service is the domain layer for projects.
type Service struct {
	store auth.Store
}

// NewService returns the project service.
func NewService(store auth.Store) *Service { return &Service{store: store} }

// Create adds a project.
//
// The (org_id, name) unique constraint makes a double-submitted form return
// the project the first submit created rather than a 409 the user cannot act
// on — but only when the base URL matches too, because "same name, different
// target" is a real conflict rather than a retry.
func (s *Service) Create(ctx context.Context, scope auth.OrgScope, name, baseURL string) (dbgen.Project, error) {
	name = strings.TrimSpace(name)
	if err := validateName(name); err != nil {
		return dbgen.Project{}, err
	}
	normalised, err := normaliseBaseURL(baseURL)
	if err != nil {
		return dbgen.Project{}, err
	}

	created, err := s.store.CreateProject(ctx, dbgen.CreateProjectParams{
		OrgID: scope.OrgID, Name: name, BaseURL: normalised,
	})
	if err == nil {
		return created, nil
	}
	if !errors.Is(db.Classify(err), db.ErrConflict) {
		return dbgen.Project{}, fmt.Errorf("create project: %w", db.Classify(err))
	}

	existing, lookupErr := s.store.GetProjectByName(ctx, dbgen.GetProjectByNameParams{OrgID: scope.OrgID, Name: name})
	if lookupErr != nil {
		return dbgen.Project{}, fmt.Errorf("look up existing project: %w", db.Classify(lookupErr))
	}
	if existing.BaseURL != normalised {
		return dbgen.Project{}, httpx.Conflict("a project called %q already targets a different url", name)
	}
	return existing, nil
}

// Get returns one project. Another organization's project is a 404.
func (s *Service) Get(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID) (dbgen.Project, error) {
	found, err := s.store.GetProject(ctx, dbgen.GetProjectParams{OrgID: scope.OrgID, ID: projectID})
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			return dbgen.Project{}, httpx.NotFound("project not found")
		}
		return dbgen.Project{}, fmt.Errorf("look up project: %w", db.Classify(err))
	}
	return found, nil
}

// Listed is one page of projects plus the total.
type Listed struct {
	Projects []dbgen.Project
	Total    int64
}

// List returns a page of an organization's projects, newest first.
func (s *Service) List(ctx context.Context, scope auth.OrgScope, includeArchived bool, page httpx.Page) (Listed, error) {
	projects, err := s.store.ListProjects(ctx, dbgen.ListProjectsParams{
		OrgID: scope.OrgID, IncludeArchived: includeArchived, Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		return Listed{}, fmt.Errorf("list projects: %w", db.Classify(err))
	}
	total, err := s.store.CountProjects(ctx, dbgen.CountProjectsParams{
		OrgID: scope.OrgID, IncludeArchived: includeArchived,
	})
	if err != nil {
		return Listed{}, fmt.Errorf("count projects: %w", db.Classify(err))
	}
	return Listed{Projects: projects, Total: total}, nil
}

// ApplicationMap assembles the application-map@1 document for a project.
//
// Three statements, whatever the map's size: pages, every element of every
// page, and workflows. Fetching elements per page would be the N+1 the
// ListElementsForProject query exists to replace.
//
// A project that discovery has never visited has no map, and the contract
// requires at least one page — an empty document would not validate as an
// application-map@1. That is a 404 rather than an empty 200: "no map yet" and
// "a map of nothing" are different states, and only one of them is true.
func (s *Service) ApplicationMap(ctx context.Context, scope auth.OrgScope, projectID uuid.UUID) (qaschema.ApplicationMap, error) {
	found, err := s.Get(ctx, scope, projectID)
	if err != nil {
		return qaschema.ApplicationMap{}, err
	}

	pages, err := s.store.ListPages(ctx, dbgen.ListPagesParams{OrgID: scope.OrgID, ProjectID: projectID})
	if err != nil {
		return qaschema.ApplicationMap{}, fmt.Errorf("list pages: %w", db.Classify(err))
	}
	elements, err := s.store.ListElementsForProject(ctx, dbgen.ListElementsForProjectParams{
		OrgID: scope.OrgID, ProjectID: projectID,
	})
	if err != nil {
		return qaschema.ApplicationMap{}, fmt.Errorf("list elements: %w", db.Classify(err))
	}
	workflows, err := s.store.ListWorkflows(ctx, dbgen.ListWorkflowsParams{OrgID: scope.OrgID, ProjectID: projectID})
	if err != nil {
		return qaschema.ApplicationMap{}, fmt.Errorf("list workflows: %w", db.Classify(err))
	}

	byPage := make(map[uuid.UUID][]qaschema.Element, len(pages))
	for _, e := range elements {
		// application-map@1 requires lastSeenRunId on every element: an element
		// nobody has observed cannot be aged out, and a planner that trusts it
		// writes steps against markup that may be long gone. Every element this
		// backend ingests is stamped, so a row without one predates a run and is
		// left out of the served document rather than rendered as un-aged.
		if !e.LastSeenRunID.Valid {
			httpx.LoggerFrom(ctx).WarnContext(ctx, "application map element has no last-seen run",
				"element_ref", e.Ref, "project_id", projectID)
			continue
		}
		element := qaschema.Element{
			Ref:           e.Ref,
			Type:          qaschema.ElementType(e.Kind),
			LastSeenRunID: e.LastSeenRunID.UUID.String(),
		}
		if e.Label != "" {
			label := e.Label
			element.Label = &label
		}
		if err := json.Unmarshal(e.Locators, &element.Locators); err != nil {
			return qaschema.ApplicationMap{}, fmt.Errorf("decode locators for element %s: %w", e.Ref, err)
		}
		byPage[e.PageID] = append(byPage[e.PageID], element)
	}

	projectRef := projectID.String()
	document := qaschema.ApplicationMap{
		Version:   1,
		BaseURL:   found.BaseURL,
		ProjectID: &projectRef,
		Pages:     make([]qaschema.Page, 0, len(pages)),
		Workflows: make([]qaschema.Workflow, 0, len(workflows)),
	}

	var latest uuid.UUID
	for _, p := range pages {
		authRequired := p.AuthRequired
		page := qaschema.Page{
			ID:           p.Ref,
			Path:         p.Path,
			Title:        p.Title,
			AuthRequired: &authRequired,
			Elements:     byPage[p.ID],
		}
		if page.Elements == nil {
			page.Elements = []qaschema.Element{}
		}
		if p.LastSeenRunID.Valid {
			ref := p.LastSeenRunID.UUID.String()
			page.LastSeenRunID = &ref
			latest = p.LastSeenRunID.UUID
		}
		document.Pages = append(document.Pages, page)
	}

	for _, w := range workflows {
		workflow := qaschema.Workflow{ID: w.Ref, Name: w.Name, ExpectedOutcome: w.ExpectedOutcome, Path: []qaschema.Ref{}}
		if err := json.Unmarshal(w.Path, &workflow.Path); err != nil {
			return qaschema.ApplicationMap{}, fmt.Errorf("decode path for workflow %s: %w", w.Ref, err)
		}
		if w.LastSeenRunID.Valid {
			ref := w.LastSeenRunID.UUID.String()
			workflow.LastSeenRunID = &ref
		}
		document.Workflows = append(document.Workflows, workflow)
	}

	if len(document.Pages) == 0 {
		return qaschema.ApplicationMap{}, httpx.NotFound(
			"this project has no application map yet; start a discover run first")
	}
	if latest != uuid.Nil {
		ref := latest.String()
		document.GeneratedAtRunID = &ref
	}
	return document, nil
}

const maxProjectNameLength = 200

func validateName(name string) error {
	if name == "" || len(name) > maxProjectNameLength {
		return httpx.InvalidField("name",
			fmt.Sprintf("must be between 1 and %d characters", maxProjectNameLength))
	}
	return nil
}

// normaliseBaseURL rejects anything the daemon could not open, and strips the
// parts of a URL that are not part of an origin.
//
// A credential in the URL is dropped rather than stored: base_url is rendered
// in the UI, handed to a daemon and written into an application map, and
// "https://admin:hunter2@staging" would end up in all three.
func normaliseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", httpx.InvalidField("baseUrl", "must be an absolute http or https url")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", httpx.InvalidField("baseUrl", "must use the http or https scheme")
	}
	if parsed.User != nil {
		return "", httpx.InvalidField("baseUrl", "must not contain credentials")
	}

	normalised := &url.URL{
		Scheme: strings.ToLower(parsed.Scheme),
		Host:   strings.ToLower(parsed.Host),
		Path:   strings.TrimSuffix(parsed.Path, "/"),
	}
	return normalised.String(), nil
}
