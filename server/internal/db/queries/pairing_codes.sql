-- One-time daemon pairing codes. Tenancy layer, like runtime_tokens.

-- name: CreatePairingCode :one
INSERT INTO pairing_codes (org_id, code_hash, created_by, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- Redemption. The UPDATE is the claim: `consumed_at IS NULL` in the predicate
-- plus the row lock the UPDATE takes means two daemons racing on the same code
-- produce exactly one winner, with no read-then-write window in between.
-- name: ConsumePairingCode :one
UPDATE pairing_codes
SET consumed_at = now(), runtime_id = sqlc.arg(runtime_id)
WHERE code_hash = sqlc.arg(code_hash)
  AND consumed_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: GetPairingCodeByHash :one
SELECT * FROM pairing_codes WHERE code_hash = $1;

-- Redemption needs the org before it can create the runtime the code will be
-- consumed against, so it reads the live row first. Expiry and prior
-- consumption are filtered here; ConsumePairingCode is still the atomic claim.
-- name: GetLivePairingCodeByHash :one
SELECT * FROM pairing_codes
WHERE code_hash = $1
  AND consumed_at IS NULL
  AND expires_at > now();

-- The org's outstanding codes, so the UI can show "a pairing code is waiting"
-- without ever being able to show the code itself.
-- name: ListLivePairingCodes :many
SELECT * FROM pairing_codes
WHERE org_id = $1 AND consumed_at IS NULL AND expires_at > now()
ORDER BY created_at DESC;

-- name: DeleteExpiredPairingCodes :execrows
DELETE FROM pairing_codes WHERE consumed_at IS NULL AND expires_at < now();
