# ADR-009: The artifact upload grant is a minting endpoint, not one prefix-wide URL

- **Status:** Accepted — 2026-09-04
- **Affected:** `server/internal/artifact`, `server/internal/run` (LONG-10),
  `daemon/artifacts` (LONG-11), `packages/qa-schema` (`daemon-envelope@1`)
- **Related:** [ADR-002](0002-daemon-outbound-only-presigned-artifacts.md)

## Context

ADR-002 settled that run evidence never travels through the API: the daemon
uploads straight to S3/MinIO with a presigned URL, and a presigned URL "is valid
only for its own run prefix". `daemon-envelope@1` encodes that as an
`ArtifactUpload { presignedPutBase, keyPrefix, expiresAt }` inside `run.assign`.

Implementing it exposed a gap. **S3 cannot issue a presigned PUT scoped to a
prefix.** A SigV4 query-string signature commits to one exact canonical URI, so
a signed URL is a capability for a single object key. The only S3 mechanisms
that bound a *prefix* are:

- a **POST form policy** with `["starts-with", "$key", "..."]` — which is a
  browser upload form, not a PUT, and would mean a second upload code path in
  the daemon; or
- **STS `AssumeRole` with an inline session policy** — MinIO supports it, but it
  moves SigV4 signing, and therefore the tenant boundary, onto the daemon, and
  adds an STS dependency to every deployment.

Meanwhile the daemon does not know its object keys in advance: a screenshot's
name is derived at execution time.

## Decision

`artifactUpload.presignedPutBase` is the URL of a **minting endpoint on this
backend**, scoped to one run:

```
POST /api/v1/runs/{runID}/artifacts/presign      Authorization: Bearer <runtime token>
{ "key": "orgs/{orgId}/runs/{YYYY-MM-DD}/{runId}/TC-001/shot.png" }
  -> 201 { "url": "...", "key": "...", "method": "PUT", "expiresAt": "..." }
```

The daemon calls it once per object and PUTs the bytes straight to storage with
the URL it gets back. The prefix bound is enforced where it can actually be
enforced, three times over:

1. `PutURL` refuses to sign a key that is not structurally inside the run's own
   `orgs/{orgID}/runs/{YYYY-MM-DD}/{runID}/` prefix — a `403`, not a signature;
2. the endpoint refuses a run that is not assigned to the caller's runtime
   (`404`) or has already finished (`409`), so the window closes with the run;
3. `artifacts_storage_key_layout` rejects a row whose `storage_key` is not under
   its own `org_id`/`run_id` when the result frame is ingested.

Every URL that leaves the backend is therefore a capability for exactly one
object inside exactly one run's prefix — a *stronger* bound than the
prefix-wide grant the contract asked for.

`keyPrefix` and `expiresAt` keep their meaning: the prefix the daemon may
compose keys under, and when the run's upload window closes. The window is
capped at six hours (`artifact.MaxUploadWindow`).

## Alternatives considered

- **Change `daemon-envelope@1` to carry per-object URLs.** Rejected: the keys do
  not exist at assignment time.
- **POST form policy.** Rejected: a second upload path in the daemon, and a
  policy document that has to be signed and reasoned about separately.
- **STS with an inline prefix policy.** Rejected for the MVP: it makes the
  daemon a SigV4 signer and adds an STS endpoint to every deployment, to buy a
  weaker bound than the one above. Worth revisiting if artifact counts per run
  make the extra round trip measurable.
- **Proxy uploads through the API.** Rejected by ADR-002.

## Consequences

- One extra round trip per artifact, against an upload of megabytes. Negligible.
- A backend outage stops uploads, where a pre-issued prefix grant would not.
  Acceptable: a run whose backend is down cannot report a result either.
- `presignedPutBase` is a backend URL, so `SERVER_PUBLIC_URL` must be an origin
  the *daemon* can reach — it is on the customer's network, not ours.
- Storage may be left unconfigured. The API still serves; only the presign
  endpoint returns `503` and reports omit download URLs.
