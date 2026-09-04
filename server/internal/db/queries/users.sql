-- Tenancy layer: see the note in organizations.sql.

-- name: CreateUser :one
INSERT INTO users (email, password_hash, name)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- The login lookup. citext makes this case-insensitive without a lower() cast
-- that one code path could forget.
-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: UpdateUserProfile :one
UPDATE users SET name = $2 WHERE id = $1 RETURNING *;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;
