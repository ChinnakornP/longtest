// Command seed populates a development database with the minimum a developer
// needs to sign in and start a run: one organization, one owner, one project.
//
//	SEED_OWNER_PASSWORD=... go run ./cmd/seed
//
// It is idempotent: running it twice leaves the same three rows, so it is safe
// to wire into a `make up && make migrate-up && make seed` loop.
//
// This command is for local development only. It refuses to invent a password:
// the owner's password comes from SEED_OWNER_PASSWORD and is never printed,
// logged or defaulted, so nothing here can turn into a shipped credential.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	pgdb "github.com/ChinnakornP/longtest/server/pkg/db"
)

const (
	defaultOrgName      = "Acme QA"
	defaultOrgSlug      = "acme"
	defaultOwnerEmail   = "owner@example.com"
	defaultOwnerName    = "Acme Owner"
	defaultProjectName  = "Demo application"
	defaultProjectURL   = "https://demo.example.com"
	minOwnerPasswordLen = 12
	seedTimeout         = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	password := os.Getenv("SEED_OWNER_PASSWORD")
	if len(password) < minOwnerPasswordLen {
		return fmt.Errorf("SEED_OWNER_PASSWORD must be set and at least %d characters; "+
			"this command never invents or defaults a password", minOwnerPasswordLen)
	}

	dsn, err := pgdb.DSNFromEnv()
	if err != nil {
		return fmt.Errorf("%w (copy .env.example to .env)", err)
	}

	ctx, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()

	pool, err := pgdb.NewPool(ctx, dsn, pgdb.DefaultPoolConfig())
	if err != nil {
		return err
	}
	defer pool.Close()

	store := db.NewStore(pool)

	// bcrypt at the default cost. The hash format is the auth task's decision
	// to revisit; the seed only has to produce something that layer accepts.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash the owner password: %w", err)
	}

	var (
		org     dbgen.Organization
		owner   dbgen.User
		project dbgen.Project
	)

	// One transaction: a half-seeded database (an org with no owner) is worse
	// than no seed at all, because the next run would find the org and stop.
	err = store.WithTx(ctx, func(q *dbgen.Queries) error {
		org, err = getOrCreateOrg(ctx, q)
		if err != nil {
			return err
		}
		owner, err = getOrCreateOwner(ctx, q, string(hash))
		if err != nil {
			return err
		}
		if _, err := q.UpsertMembership(ctx, dbgen.UpsertMembershipParams{
			OrgID:  org.ID,
			UserID: owner.ID,
			Role:   dbgen.MembershipRoleOwner,
		}); err != nil {
			return fmt.Errorf("grant owner membership: %w", db.Classify(err))
		}
		project, err = getOrCreateProject(ctx, q, org.ID)
		return err
	})
	if err != nil {
		return err
	}

	// Ids only. The password is not echoed back, not even the one the caller
	// just supplied.
	fmt.Printf("organization %s (%s)\nowner        %s (%s)\nproject      %s (%s)\n",
		org.Name, org.ID, owner.Email, owner.ID, project.Name, project.ID)
	return nil
}

func getOrCreateOrg(ctx context.Context, q *dbgen.Queries) (dbgen.Organization, error) {
	org, err := q.GetOrganizationBySlug(ctx, defaultOrgSlug)
	if err == nil {
		return org, nil
	}
	if !errors.Is(db.Classify(err), db.ErrNotFound) {
		return dbgen.Organization{}, fmt.Errorf("look up organization: %w", db.Classify(err))
	}

	org, err = q.CreateOrganization(ctx, dbgen.CreateOrganizationParams{
		Name: defaultOrgName,
		Slug: defaultOrgSlug,
	})
	if err != nil {
		return dbgen.Organization{}, fmt.Errorf("create organization: %w", db.Classify(err))
	}
	return org, nil
}

func getOrCreateOwner(ctx context.Context, q *dbgen.Queries, passwordHash string) (dbgen.User, error) {
	user, err := q.GetUserByEmail(ctx, defaultOwnerEmail)
	if err == nil {
		// Refresh the hash so a developer who changed SEED_OWNER_PASSWORD can
		// re-seed and log in with the new one.
		if _, uerr := q.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
			ID:           user.ID,
			PasswordHash: passwordHash,
		}); uerr != nil {
			return dbgen.User{}, fmt.Errorf("update owner password: %w", db.Classify(uerr))
		}
		return user, nil
	}
	if !errors.Is(db.Classify(err), db.ErrNotFound) {
		return dbgen.User{}, fmt.Errorf("look up owner: %w", db.Classify(err))
	}

	user, err = q.CreateUser(ctx, dbgen.CreateUserParams{
		Email:        defaultOwnerEmail,
		PasswordHash: passwordHash,
		Name:         defaultOwnerName,
	})
	if err != nil {
		return dbgen.User{}, fmt.Errorf("create owner: %w", db.Classify(err))
	}
	return user, nil
}

func getOrCreateProject(ctx context.Context, q *dbgen.Queries, orgID uuid.UUID) (dbgen.Project, error) {
	project, err := q.GetProjectByName(ctx, dbgen.GetProjectByNameParams{
		OrgID: orgID,
		Name:  defaultProjectName,
	})
	if err == nil {
		return project, nil
	}
	if !errors.Is(db.Classify(err), db.ErrNotFound) {
		return dbgen.Project{}, fmt.Errorf("look up project: %w", db.Classify(err))
	}

	project, err = q.CreateProject(ctx, dbgen.CreateProjectParams{
		OrgID:   orgID,
		Name:    defaultProjectName,
		BaseURL: defaultProjectURL,
	})
	if err != nil {
		return dbgen.Project{}, fmt.Errorf("create project: %w", db.Classify(err))
	}
	return project, nil
}
