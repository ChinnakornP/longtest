package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ChinnakornP/longtest/server/internal/db"
	"github.com/ChinnakornP/longtest/server/internal/db/dbgen"
	"github.com/ChinnakornP/longtest/server/internal/httpx"
)

// OrgCreator creates an organization and makes a user its owner, inside a
// transaction the caller already opened.
//
// Signup needs this, and organization creation belongs to internal/org - but
// internal/org depends on this package for Role and the middleware, so the
// dependency is inverted here rather than made circular. cmd/server wires the
// concrete implementation in.
type OrgCreator interface {
	CreateForOwner(ctx context.Context, q *dbgen.Queries, ownerID uuid.UUID, name string) (dbgen.Organization, error)
}

// Service implements signup, login, logout and /me.
//
// Everything that writes more than one row does so in a single transaction:
// signup creates a user, an organization, a membership and a session, and a
// partial application of that (an account with no org, an org with no owner)
// is a state no later request can repair.
type Service struct {
	store    Store
	hasher   *Hasher
	sessions *Sessions
	orgs     OrgCreator
}

// NewService wires the auth service.
func NewService(store Store, hasher *Hasher, sessions *Sessions, orgs OrgCreator) *Service {
	return &Service{store: store, hasher: hasher, sessions: sessions, orgs: orgs}
}

// Credentials issued by signup and login. The token is the cookie value: it is
// returned so the handler can set the cookie, and it goes nowhere else.
type Credentials struct {
	Token     string
	ExpiresAt time.Time
}

// SignupInput is the POST /api/v1/auth/signup body, already decoded.
type SignupInput struct {
	Email    string
	Password string
	Name     string
	OrgName  string
}

// SignupResult is what the endpoint returns, plus the session to set.
type SignupResult struct {
	User        dbgen.User
	Org         dbgen.Organization
	Role        Role
	Credentials Credentials
}

// Signup creates the first user of a new organization and signs them in.
//
// The account is always created together with an organization: there is no
// state in this product where a user exists but belongs to nothing, and
// allowing one would mean every later screen had to handle it.
func (s *Service) Signup(ctx context.Context, in SignupInput) (SignupResult, error) {
	email, err := NormalizeEmail(in.Email)
	if err != nil {
		return SignupResult{}, err
	}
	name, err := normalizeDisplayName(in.Name, "name")
	if err != nil {
		return SignupResult{}, err
	}
	orgName, err := normalizeDisplayName(in.OrgName, "orgName")
	if err != nil {
		return SignupResult{}, err
	}
	if err := ValidatePassword(in.Password); err != nil {
		return SignupResult{}, httpx.InvalidField("password", err.Error())
	}

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return SignupResult{}, fmt.Errorf("hash password: %w", err)
	}

	var (
		result SignupResult
		token  string
		expiry time.Time
	)
	err = s.store.WithTx(ctx, func(q *dbgen.Queries) error {
		user, err := q.CreateUser(ctx, dbgen.CreateUserParams{
			Email:        email,
			PasswordHash: hash,
			Name:         name,
		})
		if err != nil {
			if errors.Is(db.Classify(err), db.ErrConflict) {
				// Signup necessarily reveals that an address is taken - the
				// alternative is an account the user can never create or use.
				// Login does not, which is where it matters.
				return httpx.Conflict("an account with that e-mail already exists")
			}
			return fmt.Errorf("create user: %w", db.Classify(err))
		}

		org, err := s.orgs.CreateForOwner(ctx, q, user.ID, orgName)
		if err != nil {
			return err
		}

		token, expiry, err = s.sessions.Issue(ctx, q, user.ID)
		if err != nil {
			return err
		}

		result = SignupResult{User: user, Org: org, Role: RoleOwner}
		return nil
	})
	if err != nil {
		return SignupResult{}, err
	}

	result.Credentials = Credentials{Token: token, ExpiresAt: expiry}
	return result, nil
}

// LoginResult is what a successful login returns.
type LoginResult struct {
	User        dbgen.User
	Orgs        []Membership
	Credentials Credentials
}

// Membership is one row of the org picker: an organization plus the caller's
// role in it.
type Membership struct {
	Org  dbgen.Organization
	Role Role
}

// Login verifies a password and issues a session.
//
// An unknown e-mail and a wrong password are the same error and take
// comparable time: the dummy verification below is what keeps this endpoint
// from being a user-enumeration oracle.
func (s *Service) Login(ctx context.Context, email, password string) (LoginResult, error) {
	normalised, err := NormalizeEmail(email)
	if err != nil {
		// Not a valid address, so it cannot match a stored one. Answer exactly
		// as for a wrong password.
		s.hasher.VerifyDummy(password)
		return LoginResult{}, ErrInvalidCredentials
	}

	user, err := s.store.GetUserByEmail(ctx, normalised)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			s.hasher.VerifyDummy(password)
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, fmt.Errorf("look up user: %w", db.Classify(err))
	}

	if err := s.hasher.Verify(user.PasswordHash, password); err != nil {
		// Includes ErrUnsupportedHash: a row we cannot verify is not a login.
		if errors.Is(err, ErrUnsupportedHash) {
			httpx.LoggerFrom(ctx).ErrorContext(ctx, "stored password hash is unusable",
				"user_id", user.ID.String())
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	// The password is correct, so this is the one moment we can re-hash it at
	// the current cost. Failing to upgrade must not fail the login.
	if s.hasher.NeedsRehash(user.PasswordHash) {
		s.upgradeHash(ctx, user.ID, password)
	}

	token, expiry, err := s.sessions.Issue(ctx, s.store, user.ID)
	if err != nil {
		return LoginResult{}, err
	}

	orgs, err := s.membershipsFor(ctx, user.ID)
	if err != nil {
		return LoginResult{}, err
	}

	return LoginResult{
		User:        user,
		Orgs:        orgs,
		Credentials: Credentials{Token: token, ExpiresAt: expiry},
	}, nil
}

// upgradeHash re-hashes a verified password at the current cost parameters.
func (s *Service) upgradeHash(ctx context.Context, userID uuid.UUID, password string) {
	hash, err := s.hasher.Hash(password)
	if err != nil {
		return
	}
	if _, err := s.store.UpdateUserPassword(ctx, dbgen.UpdateUserPasswordParams{
		ID:           userID,
		PasswordHash: hash,
	}); err != nil {
		httpx.LoggerFrom(ctx).WarnContext(ctx, "could not upgrade password hash",
			"user_id", userID.String(), "err", db.Classify(err))
	}
}

// Logout revokes the session behind the cookie. It is idempotent.
func (s *Service) Logout(ctx context.Context, token string) error {
	return s.sessions.Revoke(ctx, token)
}

// MeResult is the GET /api/v1/me payload.
type MeResult struct {
	User dbgen.User
	Orgs []Membership
}

// Me returns the caller and every organization they belong to, with the role
// in each. It is what the web app calls on boot to populate the org switcher.
func (s *Service) Me(ctx context.Context, caller Caller) (MeResult, error) {
	user, err := s.store.GetUser(ctx, caller.userID)
	if err != nil {
		if errors.Is(db.Classify(err), db.ErrNotFound) {
			// The account was deleted while the session was live.
			return MeResult{}, ErrInvalidCredentials
		}
		return MeResult{}, fmt.Errorf("look up user: %w", db.Classify(err))
	}

	orgs, err := s.membershipsFor(ctx, caller.userID)
	if err != nil {
		return MeResult{}, err
	}
	return MeResult{User: user, Orgs: orgs}, nil
}

func (s *Service) membershipsFor(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := s.store.ListOrganizationsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", db.Classify(err))
	}

	out := make([]Membership, 0, len(rows))
	for _, row := range rows {
		out = append(out, Membership{
			Org: dbgen.Organization{
				ID:        row.ID,
				Name:      row.Name,
				Slug:      row.Slug,
				CreatedAt: row.CreatedAt,
				UpdatedAt: row.UpdatedAt,
			},
			Role: RoleFromDB(row.Role),
		})
	}
	return out, nil
}

// MaxEmailLength bounds the address we will store. RFC 5321 caps a path at
// 254 characters; anything longer cannot be delivered to anyway.
const MaxEmailLength = 254

// NormalizeEmail validates an address and returns the form to store.
//
// The column is citext, so case is already insignificant for uniqueness and
// lookup; lowercasing here just keeps what is displayed consistent with what
// was typed at signup.
func NormalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", httpx.InvalidField("email", "is required")
	}
	if len(trimmed) > MaxEmailLength {
		return "", httpx.InvalidField("email", fmt.Sprintf("must be at most %d characters", MaxEmailLength))
	}

	addr, err := mail.ParseAddress(trimmed)
	// mail.ParseAddress accepts `Name <a@b>`; only a bare address is an
	// identity here, so anything with a display name is rejected.
	if err != nil || addr.Name != "" || addr.Address != trimmed {
		return "", httpx.InvalidField("email", "must be an e-mail address")
	}
	if strings.Count(addr.Address, "@") != 1 {
		return "", httpx.InvalidField("email", "must be an e-mail address")
	}
	return strings.ToLower(addr.Address), nil
}

// normalizeDisplayName trims and bounds a human-supplied name. The bounds
// match the CHECK constraints in the schema, so a value that gets this far
// cannot be rejected by the database.
func normalizeDisplayName(raw, field string) (string, error) {
	const maxLen = 200
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return "", httpx.InvalidField(field, "is required")
	case len([]rune(trimmed)) > maxLen:
		return "", httpx.InvalidField(field, fmt.Sprintf("must be at most %d characters", maxLen))
	default:
		return trimmed, nil
	}
}
