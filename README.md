# S3 API Compatibility Tests

Tests S3 API operations across multiple providers using the AWS Go SDK v2. Produces a structured JSON report showing which operations pass or fail on each provider.

Every test exercises an operation and then checks its effect: a write is read back, a condition that must fail is sent and must fail with the S3 status or error code, and a delete is followed by a read that must be 404. A provider that accepts a request and ignores it does not pass.

## Supported Providers

| Provider | Endpoint | Auth |
|----------|----------|------|
| **Tigris** | `https://t3.storage.dev` | Access key + secret |
| **AWS S3** | Default | Access key + secret |
| **Cloudflare R2** | `https://<account-id>.r2.cloudflarestorage.com` | Access key + secret |
| **Google Cloud Storage** | `https://storage.googleapis.com` | HMAC key + secret |

## Setup

```bash
cp .env.example .env
# Fill in credentials for the providers you want to test
```

### Environment Variables

```bash
# Tigris
TIGRIS_ACCESS_KEY_ID=
TIGRIS_SECRET_ACCESS_KEY=
TIGRIS_STS_ENDPOINT=             # optional, defaults to https://sts.storage.dev

# AWS S3
AWS_S3_ACCESS_KEY_ID=
AWS_S3_SECRET_ACCESS_KEY=
AWS_S3_REGION=us-east-1          # optional, defaults to us-east-1

# Cloudflare R2
R2_ACCESS_KEY_ID=
R2_SECRET_ACCESS_KEY=
R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com

# Google Cloud Storage (S3-compatible HMAC)
GCS_ACCESS_KEY_ID=               # Create HMAC keys in GCS console
GCS_SECRET_ACCESS_KEY=
```

Only providers with both access key and secret key set will be tested.

## Usage

```bash
# Test all configured providers
go run .

# Test specific providers
go run . --providers=tigris,aws

# Run a subset of tests, in suite order
go run . --providers=tigris --tests=PutObject,GetObject,RenameObject

# Custom output path
go run . --providers=tigris --output=tigris-results.json
```

Each run creates a bucket named `s3compat-<provider>-<random>` and a second bucket `…-v` with object versioning enabled, runs every test, and deletes both buckets. Every test creates what it needs and cleans up after itself, so any subset selected with `--tests` runs on its own.

## How the versioned bucket is created

Versioning tests run against the second bucket. The runner creates it the way the provider supports:

- On AWS, R2 and GCS: `CreateBucket`, then `PutBucketVersioning` with `Status=Enabled`.
- On Tigris, versioning is a creation-time property, so `CreateBucket` is sent with the `X-Tigris-Enable-Snapshot: true` header (`Provider.VersionedBucketHeaders`).

If the provider cannot create a versioned bucket, the versioning tests are reported as `skip` with the reason.

The separate `PutBucketVersioning` test in the bucket category turns versioning on for the plain test bucket. Providers where versioning cannot be enabled after creation fail that one test only.

## How temporary credentials are tested

The sts category calls the provider's AWS STS endpoint with the long-lived key: `GetFederationToken` with an inline policy that names the test bucket. It then signs S3 requests with the returned access key, secret and session token, and checks that S3 allows what the policy names and refuses the rest.

- On Tigris the STS endpoint is `https://sts.storage.dev` (`TIGRIS_STS_ENDPOINT` overrides it).
- On AWS it is the STS endpoint of `AWS_S3_REGION`. The key must belong to an IAM user that may call `sts:GetFederationToken`.
- R2 and GCS have no STS endpoint, so the category is reported as `skip`.

A full run mints three sessions: one in the GetFederationToken test, and a read-write and a read-only session that the session tests share. Each lasts 900 seconds, the shortest lifetime STS issues. Expiry is not tested, because it would take 15 minutes.

## Consistency expectations

Object operations are checked immediately: S3 gives read-after-write and read-after-delete consistency for objects, so a GET after a DELETE must be 404 at once.

Bucket configuration operations (tagging, CORS, lifecycle, policy, ACL, encryption, ownership controls, accelerate, versioning) are documented by S3 as eventually consistent. Their read-backs retry for up to 15 seconds (`propagation` in `tests/env.go`) before failing.

A GET after a bucket-configuration delete must fail with the S3 error code for a missing configuration (`NoSuchTagSet`, `NoSuchCORSConfiguration`, ...), not merely with a 404. If a bucket-level delete removes the bucket itself, the test fails for that reason and re-creates the bucket so later tests still run.

## Operations Tested

**111 tests** across 6 categories:

### Object (37)
PutObject (round trip of ETag and length; metadata; SHA-256 checksum echoed; a wrong SHA-256 checksum rejected; SSE-S3 reported back; a non-default storage class reported back), GetObject (body; missing key is 404 NoSuchKey; Range with Content-Range), HeadObject, DeleteObject (gone afterwards; deleting a missing key succeeds), CopyObject (body; MetadataDirective=REPLACE; x-amz-copy-source-if-match with a matching and a wrong ETag; conditional copy of a multipart object with its composite ETag), RenameObject (source gone, destination has the body; If-None-Match: * on an existing destination is 412; a source with a leading slash or the bucket name in front is a different key: 404 NoSuchKey when it does not exist, renamed when it does), conditional GETs (If-Match, If-None-Match, If-Modified-Since, If-Unmodified-Since, each with a case that must read and a case that must be 304 or 412), conditional writes (PutObject with If-None-Match: * and with If-Match, each with the refused case), object tagging (put read back, get, delete leaves no tags), object ACL (put read back, get), ListObjects, ListObjectsV2 (basic with sizes and ETags, pagination to the end, delimiter with common prefixes and a top-level key, start-after), DeleteObjects (verbose and quiet, objects gone), RestoreObject

### Bucket (33)
HeadBucket (and 404 for a missing bucket), ListBuckets, GetBucketLocation, CreateBucket (visible afterwards), DeleteBucket (404 afterwards), tagging (put read back, get, delete then 404 NoSuchTagSet), CORS (put read back, get, delete then 404 NoSuchCORSConfiguration), lifecycle (put read back with id, prefix and days; get; delete then 404 NoSuchLifecycleConfiguration), ACL (put read back with no public grant, get), policy (put, get, delete then 404 NoSuchBucketPolicy, status not public), PutBucketVersioning on the plain bucket, accelerate configuration (put Suspended read back), request payment (BucketOwner), encryption (put, get AES256, delete then 404), ownership controls (put, get BucketOwnerEnforced, delete then 404), notification configuration (put empty, get empty)

### Versioning (10), on the versioned bucket
GetBucketVersioning reports Enabled, PutBucketVersioning Enabled again succeeds, PutObject returns distinct version ids and the current body, GetObject and HeadObject by versionId, ListObjectVersions newest first with one IsLatest, DeleteObject by versionId removes that version only, object tagging by versionId (put tags one version only and creates no version; get per version; delete one version's tags only)

### Multipart (8)
CreateMultipartUpload (listed afterwards), UploadPart (ETag), ListParts (number, size, ETag), CompleteMultipartUpload (full length, composite ETag, bytes at the part boundary), CompleteMultipartUpload with parts out of order is InvalidPartOrder, AbortMultipartUpload (ListParts is then NoSuchUpload), ListMultipartUploads, UploadPartCopy (completed object has the source length)

### Presigned URLs (6)
PresignPutObject (stored), PresignGetObject (body), PresignHeadObject (Content-Length), PresignDeleteObject (gone afterwards), PresignUploadPart (part listed), PresignPostObject (browser form upload stored)

### STS (17), on a provider with an STS endpoint
GetCallerIdentity, GetFederationToken (credentials with a session token, expiration 900 seconds away, the federated user named; DurationSeconds below 900 is ValidationError; a policy that is not JSON is MalformedPolicyDocument), a session calling GetCallerIdentity (federated user ARN), PutObject (read back with the long-lived key), GetObject and HeadObject, ListObjectsV2, DeleteObject (gone afterwards), a multipart upload, a presigned GET (the URL carries X-Amz-Security-Token), and the refusals: an action the policy does not name (AccessDenied, nothing stored), a second bucket the policy does not name (AccessDenied), ListBuckets (AccessDenied), the session key without its token (InvalidAccessKeyId), an altered token (InvalidToken), GetFederationToken signed with a session (AccessDenied)

## Output Format

```json
{
  "generated_at": "2026-04-07T15:30:00Z",
  "providers": [
    {
      "name": "tigris",
      "endpoint": "https://t3.storage.dev",
      "region": "auto",
      "total": 93,
      "passed": 80,
      "failed": 13,
      "skipped": 0,
      "errors": 0,
      "results": [
        {
          "operation": "PutObject",
          "category": "object",
          "status": "pass",
          "duration_ms": 120
        },
        {
          "operation": "PutBucketPolicy",
          "category": "bucket",
          "status": "fail",
          "duration_ms": 45,
          "error": "operation error S3: PutBucketPolicy, https response error StatusCode: 501, ..."
        }
      ]
    }
  ]
}
```

`status` is one of `pass`, `fail`, `skip` (the test did not apply, with the reason in `notes`), or `error` (test setup failed, so the operation was not judged).
