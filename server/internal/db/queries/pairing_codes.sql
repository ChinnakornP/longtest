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

-- name: DeleteExpiredPairingCodes :execrows
DELETE FROM pairing_codes WHERE consumed_at IS NULL AND expires_at < now();
