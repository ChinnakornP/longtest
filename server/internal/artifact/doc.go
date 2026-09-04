// Package artifact issues the presigned object-storage URLs that keep run
// evidence out of the API's request path (ADR-002).
//
// A trace.zip is routinely tens of megabytes and a run has one per failing
// test case, so the daemon uploads straight to S3/MinIO and the backend only
// ever handles the metadata. The two directions are:
//
//	daemon -> storage   presigned PUT,  minted per object by PutURL
//	web    <- storage   presigned GET,  minted per object by GetURL
//
// # Why a presigned PUT is minted per object and not per prefix
//
// The daemon-envelope contract hands a runtime an ArtifactUpload{
// presignedPutBase, keyPrefix, expiresAt} and says the daemon may only write
// below keyPrefix. SigV4 query-string presigning cannot express that: the
// signature covers one exact canonical URI, so a single signed URL is a
// capability for a single key. Nothing in S3 grants "PUT anything under this
// prefix" short of a POST form policy (which is not a PUT) or STS credentials
// with an inline policy (which MinIO supports but which moves signing, and
// therefore the tenant boundary, onto the daemon).
//
// So presignedPutBase is the URL of THIS package's minting endpoint, scoped to
// one run, and the prefix bound is enforced where it can actually be enforced:
// PutURL refuses to sign a key that is not under the run's own
// orgs/{orgID}/runs/{date}/{runID}/ prefix, and the artifacts_storage_key_layout
// CHECK refuses to record one. Every URL that leaves this package is therefore
// a capability for exactly one object inside one run's prefix.
package artifact
