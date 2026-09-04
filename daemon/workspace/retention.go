package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Swept describes one workspace the retention sweep deleted.
type Swept struct {
	ProjectID string
	RunID     string
	Dir       string
	Reason    string
}

// Sweep deletes finished workspaces the retention policy no longer covers and
// returns what it removed.
//
// It never deletes a workspace that has not been finished, whatever its age:
// an unfinished workspace either belongs to a run in flight or to a run whose
// daemon died mid-flight, and both are cases where deleting the evidence is
// the wrong default. Errors on individual directories are collected rather
// than aborting the sweep — one unreadable workspace must not pin every other.
func (m *Manager) Sweep() ([]Swept, error) {
	entries, err := m.finished()
	if err != nil {
		return nil, err
	}

	now := m.now().UTC()
	var (
		removed []Swept
		errs    []error
		keep    []finishedWorkspace
	)

	for _, entry := range entries {
		ttl := m.retention.KeepCompleted
		if entry.outcome != OutcomeCompleted {
			ttl = m.retention.KeepFailed
		}
		if ttl > 0 && now.Sub(entry.finishedAt) < ttl {
			keep = append(keep, entry)
			continue
		}
		if err := os.RemoveAll(entry.dir); err != nil {
			errs = append(errs, fmt.Errorf("workspace: remove %s: %w", entry.dir, err))
			continue
		}
		removed = append(removed, Swept{
			ProjectID: entry.projectID,
			RunID:     entry.runID,
			Dir:       entry.dir,
			Reason:    fmt.Sprintf("%s workspace older than %s", entry.outcome, ttl),
		})
	}

	// The age rule alone cannot bound disk on a machine that runs hundreds of
	// tests an hour, so a count cap applies on top of it, oldest first.
	if limit := m.retention.MaxRuns; limit > 0 && len(keep) > limit {
		slices.SortFunc(keep, func(a, b finishedWorkspace) int { return a.finishedAt.Compare(b.finishedAt) })
		for _, entry := range keep[:len(keep)-limit] {
			if err := os.RemoveAll(entry.dir); err != nil {
				errs = append(errs, fmt.Errorf("workspace: remove %s: %w", entry.dir, err))
				continue
			}
			removed = append(removed, Swept{
				ProjectID: entry.projectID,
				RunID:     entry.runID,
				Dir:       entry.dir,
				Reason:    fmt.Sprintf("more than %d finished workspaces kept", limit),
			})
		}
	}

	m.pruneEmptyProjects()
	return removed, errors.Join(errs...)
}

type finishedWorkspace struct {
	projectID  string
	runID      string
	dir        string
	outcome    Outcome
	finishedAt time.Time
}

func (m *Manager) finished() ([]finishedWorkspace, error) {
	projects, err := os.ReadDir(m.root)
	if err != nil {
		return nil, fmt.Errorf("workspace: read root: %w", err)
	}

	var out []finishedWorkspace
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(m.root, project.Name())
		runs, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, run := range runs {
			if !run.IsDir() {
				continue
			}
			dir := filepath.Join(projectDir, run.Name())
			info, err := readMeta(dir)
			if err != nil || info.FinishedAt == nil {
				// Unreadable metadata is treated as "still running": see Sweep.
				continue
			}
			out = append(out, finishedWorkspace{
				projectID:  project.Name(),
				runID:      run.Name(),
				dir:        dir,
				outcome:    info.Outcome,
				finishedAt: info.FinishedAt.UTC(),
			})
		}
	}
	return out, nil
}

// pruneEmptyProjects removes project directories left with no runs, so the
// root does not accumulate one empty directory per project forever.
func (m *Manager) pruneEmptyProjects() {
	projects, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(m.root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		_ = os.Remove(dir)
	}
}
