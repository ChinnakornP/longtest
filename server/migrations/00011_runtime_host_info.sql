-- The daemon reports what machine it is on when it redeems a pairing code, so
-- the runtime list can say "MacBook Pro / darwin / arm64" before the daemon has
-- ever opened a WebSocket and sent its first `hello`.
--
-- Additive only: the column has a default, so the deployed backend that does
-- not know about it keeps working (expand -> migrate -> contract).

-- +goose Up

ALTER TABLE runtimes
    ADD COLUMN host_info jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(host_info) = 'object');

COMMENT ON COLUMN runtimes.host_info IS
    'Machine facts reported at pairing time (hostname, os, arch). Rendered, never queried into.';

-- +goose Down

ALTER TABLE runtimes DROP COLUMN IF EXISTS host_info;
