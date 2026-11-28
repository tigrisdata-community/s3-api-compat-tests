package tests

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectLockTests run against env.LockBucket, a bucket the runner created with
// S3 Object Lock enabled. They skip when the runner could not create one.
//
// Every retention the tests set is GOVERNANCE, which the bucket owner may
// bypass, so each test can delete what it locked. COMPLIANCE retention cannot
// be removed before it runs out and is not exercised.
func ObjectLockTests() []TestDef {
	return []TestDef{
		{"GetObjectLockConfiguration", "object-lock", testGetObjectLockConfiguration},
		{"GetObjectLockConfiguration_NotConfigured", "object-lock", testGetObjectLockConfigurationNotConfigured},
		{"PutObjectRetention", "object-lock", testPutObjectRetention},
		{"PutObjectRetention_PastDate", "object-lock", testPutObjectRetentionPastDate},
		{"PutObjectRetention_NoLockBucket", "object-lock", testPutObjectRetentionNoLockBucket},
		{"PutObjectRetention_ShortenNeedsBypass", "object-lock", testPutObjectRetentionShortenNeedsBypass},
		{"PutObject_LockHeaders", "object-lock", testPutObjectLockHeaders},
		{"DeleteObject_LockedVersion", "object-lock", testDeleteObjectLockedVersion},
		{"DeleteObject_BypassGovernance", "object-lock", testDeleteObjectBypassGovernance},
		{"DeleteObjects_LockedVersion", "object-lock", testDeleteObjectsLockedVersion},
		{"PutObjectLegalHold", "object-lock", testPutObjectLegalHold},
		{"DeleteObject_LegalHold", "object-lock", testDeleteObjectLegalHold},
		{"PutObjectLockConfiguration_DefaultRetention", "object-lock", testPutObjectLockConfigurationDefaultRetention},
	}
}

func needLock(env *Env) *TestResult {
	if env.LockBucket == "" {
		r := Skip("no object lock bucket: " + env.LockBucketErr)
		return &r
	}
	return nil
}

// retainFor returns a retain-until date d from now, to the second: S3 does not
// promise to keep more precision than that.
func retainFor(d time.Duration) time.Time {
	return time.Now().Add(d).UTC().Truncate(time.Second)
}

// lockPut writes key in the lock bucket and returns the version id.
func lockPut(ctx context.Context, env *Env, key string) (string, error) {
	out, err := putBody(ctx, env, env.LockBucket, key, "locked")
	if err != nil {
		return "", err
	}
	v := aws.ToString(out.VersionId)
	if v == "" {
		return "", fmt.Errorf("PutObject in an object lock bucket returned no version id")
	}
	return v, nil
}

func setGovernance(ctx context.Context, env *Env, key, versionId string, until time.Time, bypass bool) error {
	in := &s3.PutObjectRetentionInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(versionId),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until)},
	}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	_, err := env.Client.PutObjectRetention(ctx, in)
	return err
}

func setLegalHold(ctx context.Context, env *Env, key, versionId string, status types.ObjectLockLegalHoldStatus) error {
	_, err := env.Client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(versionId),
		LegalHold: &types.ObjectLockLegalHold{Status: status},
	})
	return err
}

// checkRetention reads the retention of a version back and compares it.
func checkRetention(ctx context.Context, env *Env, key, versionId string, until time.Time) TestResult {
	out, err := env.Client.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(versionId),
	})
	if err != nil {
		return Fail("GetObjectRetention: " + err.Error())
	}
	if out.Retention == nil {
		return Fail("GetObjectRetention returned no retention")
	}
	if out.Retention.Mode != types.ObjectLockRetentionModeGovernance {
		return Fail(fmt.Sprintf("retention mode %q, want GOVERNANCE", out.Retention.Mode))
	}
	got := aws.ToTime(out.Retention.RetainUntilDate).UTC().Truncate(time.Second)
	if !got.Equal(until) {
		return Fail(fmt.Sprintf("retain until %s, want %s", got.Format(time.RFC3339), until.Format(time.RFC3339)))
	}
	return Pass()
}

// versionExists reports whether a version can still be read.
func versionExists(ctx context.Context, env *Env, key, versionId string) (bool, error) {
	_, err := env.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(versionId),
	})
	if err == nil {
		return true, nil
	}
	if httpStatus(err) == 404 {
		return false, nil
	}
	return false, err
}

// lockCleanup removes every version and delete marker of key from the lock
// bucket: it turns a legal hold off and deletes with the governance bypass.
func lockCleanup(ctx context.Context, env *Env, key string) {
	out, err := env.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: aws.String(env.LockBucket), Prefix: aws.String(key),
	})
	if err != nil {
		return
	}
	for _, v := range out.Versions {
		if aws.ToString(v.Key) != key {
			continue
		}
		_ = setLegalHold(ctx, env, key, aws.ToString(v.VersionId), types.ObjectLockLegalHoldStatusOff)
		_, _ = env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: v.VersionId,
			BypassGovernanceRetention: aws.Bool(true),
		})
	}
	for _, m := range out.DeleteMarkers {
		if aws.ToString(m.Key) != key {
			continue
		}
		_, _ = env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: m.VersionId,
		})
	}
}

// A bucket created with Object Lock reports it enabled, and has versioning on.
func testGetObjectLockConfiguration(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	out, err := env.Client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String(env.LockBucket),
	})
	if err != nil {
		return Fail(err.Error())
	}
	if out.ObjectLockConfiguration == nil || out.ObjectLockConfiguration.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		return Fail("ObjectLockEnabled is not Enabled")
	}
	ver, err := env.Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(env.LockBucket)})
	if err != nil {
		return Fail("GetBucketVersioning: " + err.Error())
	}
	if ver.Status != types.BucketVersioningStatusEnabled {
		return Fail(fmt.Sprintf("versioning status %q on an object lock bucket, want Enabled", ver.Status))
	}
	return Pass()
}

// A bucket without Object Lock answers ObjectLockConfigurationNotFoundError.
func testGetObjectLockConfigurationNotConfigured(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	_, err := env.Client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String(env.Bucket),
	})
	return expectFailure(err, "GetObjectLockConfiguration on a bucket without Object Lock", 0, "ObjectLockConfigurationNotFoundError")
}

// PutObjectRetention sets a retention that GetObjectRetention and HeadObject
// report, and a later date extends it without the bypass.
func testPutObjectRetention(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-retention"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	until := retainFor(time.Hour)
	if err := setGovernance(ctx, env, key, v, until, false); err != nil {
		return Fail("PutObjectRetention: " + err.Error())
	}
	if r := checkRetention(ctx, env, key, v, until); r.Status != StatusPass {
		return r
	}
	head, err := env.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
	})
	if err != nil {
		return Fail("HeadObject: " + err.Error())
	}
	if head.ObjectLockMode != types.ObjectLockModeGovernance {
		return Fail(fmt.Sprintf("HeadObject x-amz-object-lock-mode %q, want GOVERNANCE", head.ObjectLockMode))
	}
	if got := aws.ToTime(head.ObjectLockRetainUntilDate).UTC().Truncate(time.Second); !got.Equal(until) {
		return Fail(fmt.Sprintf("HeadObject x-amz-object-lock-retain-until-date %s, want %s", got.Format(time.RFC3339), until.Format(time.RFC3339)))
	}
	later := retainFor(2 * time.Hour)
	if err := setGovernance(ctx, env, key, v, later, false); err != nil {
		return Fail("extending a governance retention without the bypass: " + err.Error())
	}
	return checkRetention(ctx, env, key, v, later)
}

// A retain-until date that is not in the future is refused.
func testPutObjectRetentionPastDate(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-retention-past"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	err = setGovernance(ctx, env, key, v, retainFor(-time.Hour), false)
	return expectFailure(err, "PutObjectRetention with a date in the past", 400, "InvalidArgument")
}

// PutObjectRetention on a bucket without Object Lock is refused, and the
// object stays deletable.
func testPutObjectRetentionNoLockBucket(ctx context.Context, env *Env) TestResult {
	key := "lock-retention-plain-bucket"
	if err := putTestObject(ctx, env, key); err != nil {
		return Error("setup: " + err.Error())
	}
	defer deleteObject(ctx, env, key)
	_, err := env.Client.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key),
		Retention: &types.ObjectLockRetention{
			Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(retainFor(time.Hour)),
		},
	})
	return expectFailure(err, "PutObjectRetention on a bucket without Object Lock", 400, "InvalidRequest")
}

// Shortening a governance retention needs the bypass: without it the change is
// AccessDenied and the stored date is unchanged, with it the change is made.
func testPutObjectRetentionShortenNeedsBypass(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-retention-shorten"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	until := retainFor(2 * time.Hour)
	if err := setGovernance(ctx, env, key, v, until, false); err != nil {
		return Error("setup: PutObjectRetention: " + err.Error())
	}
	shorter := retainFor(time.Hour)
	err = setGovernance(ctx, env, key, v, shorter, false)
	if r := expectFailure(err, "shortening a governance retention without the bypass", 403, "AccessDenied"); r.Status != StatusPass {
		return r
	}
	if r := checkRetention(ctx, env, key, v, until); r.Status != StatusPass {
		return Fail("after the refused change: " + r.Error)
	}
	if err := setGovernance(ctx, env, key, v, shorter, true); err != nil {
		return Fail("shortening a governance retention with the bypass: " + err.Error())
	}
	return checkRetention(ctx, env, key, v, shorter)
}

// The lock headers of a PutObject set the retention and the legal hold of the
// version it writes.
func testPutObjectLockHeaders(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-put-headers"
	defer lockCleanup(ctx, env, key)
	until := retainFor(time.Hour)
	out, err := env.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), Body: strings.NewReader("locked"),
		ObjectLockMode:            types.ObjectLockModeGovernance,
		ObjectLockRetainUntilDate: aws.Time(until),
		ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	})
	if err != nil {
		return Fail("PutObject with lock headers: " + err.Error())
	}
	v := aws.ToString(out.VersionId)
	if v == "" {
		return Fail("PutObject returned no version id")
	}
	if r := checkRetention(ctx, env, key, v, until); r.Status != StatusPass {
		return r
	}
	hold, err := env.Client.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
	})
	if err != nil {
		return Fail("GetObjectLegalHold: " + err.Error())
	}
	if hold.LegalHold == nil || hold.LegalHold.Status != types.ObjectLockLegalHoldStatusOn {
		return Fail("legal hold is not ON after a PutObject that set it")
	}
	return Pass()
}

// A version under governance retention cannot be deleted without the bypass.
// A delete that names no version still succeeds: it adds a delete marker and
// removes nothing.
func testDeleteObjectLockedVersion(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-delete-denied"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	if err := setGovernance(ctx, env, key, v, retainFor(time.Hour), false); err != nil {
		return Error("setup: PutObjectRetention: " + err.Error())
	}
	_, err = env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
	})
	if r := expectFailure(err, "DeleteObject of a locked version", 403, "AccessDenied"); r.Status != StatusPass {
		return r
	}
	out, err := env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key),
	})
	if err != nil {
		return Fail("DeleteObject without a version id on a locked object: " + err.Error())
	}
	if !aws.ToBool(out.DeleteMarker) {
		return Fail("DeleteObject without a version id on a locked object did not report a delete marker")
	}
	_, err = env.Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key),
	})
	if r := expectFailure(err, "HeadObject of the key after the delete marker", 404); r.Status != StatusPass {
		return r
	}
	ok, err := versionExists(ctx, env, key, v)
	if err != nil {
		return Fail("reading the locked version back: " + err.Error())
	}
	if !ok {
		return Fail("the locked version is gone after the deletes")
	}
	return Pass()
}

// The governance bypass deletes a version under governance retention.
func testDeleteObjectBypassGovernance(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-delete-bypass"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	until := retainFor(time.Hour)
	if err := setGovernance(ctx, env, key, v, until, false); err != nil {
		return Error("setup: PutObjectRetention: " + err.Error())
	}
	if r := checkRetention(ctx, env, key, v, until); r.Status != StatusPass {
		return r
	}
	if _, err := env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
		BypassGovernanceRetention: aws.Bool(true),
	}); err != nil {
		return Fail("DeleteObject with the governance bypass: " + err.Error())
	}
	ok, err := versionExists(ctx, env, key, v)
	if err != nil {
		return Fail("reading the version back: " + err.Error())
	}
	if ok {
		return Fail("the version is still there after a delete with the governance bypass")
	}
	return Pass()
}

// DeleteObjects reports a locked version as an AccessDenied error for that key
// and deletes the unlocked one beside it; with the bypass it deletes both.
func testDeleteObjectsLockedVersion(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	lockedKey, freeKey := "lock-multi-locked", "lock-multi-free"
	defer lockCleanup(ctx, env, lockedKey)
	defer lockCleanup(ctx, env, freeKey)
	lockedV, err := lockPut(ctx, env, lockedKey)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	freeV, err := lockPut(ctx, env, freeKey)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	if err := setGovernance(ctx, env, lockedKey, lockedV, retainFor(time.Hour), false); err != nil {
		return Error("setup: PutObjectRetention: " + err.Error())
	}
	objects := []types.ObjectIdentifier{
		{Key: aws.String(lockedKey), VersionId: aws.String(lockedV)},
		{Key: aws.String(freeKey), VersionId: aws.String(freeV)},
	}
	out, err := env.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(env.LockBucket), Delete: &types.Delete{Objects: objects},
	})
	if err != nil {
		return Fail("DeleteObjects: " + err.Error())
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != lockedKey || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		return Fail(fmt.Sprintf("expected one AccessDenied error for %s, got %d errors: %+v", lockedKey, len(out.Errors), describeDeleteErrors(out.Errors)))
	}
	if ok, err := versionExists(ctx, env, lockedKey, lockedV); err != nil || !ok {
		return Fail(fmt.Sprintf("the locked version is not readable after DeleteObjects (exists=%v, err=%v)", ok, err))
	}
	if ok, err := versionExists(ctx, env, freeKey, freeV); err != nil || ok {
		return Fail(fmt.Sprintf("the unlocked version was not deleted (exists=%v, err=%v)", ok, err))
	}
	out, err = env.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(env.LockBucket), Delete: &types.Delete{Objects: objects[:1]},
		BypassGovernanceRetention: aws.Bool(true),
	})
	if err != nil {
		return Fail("DeleteObjects with the governance bypass: " + err.Error())
	}
	if len(out.Errors) != 0 {
		return Fail(fmt.Sprintf("DeleteObjects with the governance bypass reported errors: %+v", describeDeleteErrors(out.Errors)))
	}
	if ok, err := versionExists(ctx, env, lockedKey, lockedV); err != nil || ok {
		return Fail(fmt.Sprintf("the locked version was not deleted with the bypass (exists=%v, err=%v)", ok, err))
	}
	return Pass()
}

func describeDeleteErrors(errs []types.Error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = fmt.Sprintf("%s: %s %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
	}
	return out
}

// PutObjectLegalHold turns a hold on and off, and GetObjectLegalHold and
// HeadObject report it.
func testPutObjectLegalHold(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-legal-hold"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	for _, want := range []types.ObjectLockLegalHoldStatus{types.ObjectLockLegalHoldStatusOn, types.ObjectLockLegalHoldStatusOff} {
		if err := setLegalHold(ctx, env, key, v, want); err != nil {
			return Fail(fmt.Sprintf("PutObjectLegalHold %s: %v", want, err))
		}
		out, err := env.Client.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{
			Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
		})
		if err != nil {
			return Fail("GetObjectLegalHold: " + err.Error())
		}
		if out.LegalHold == nil || out.LegalHold.Status != want {
			return Fail(fmt.Sprintf("legal hold read back differs from %s", want))
		}
		head, err := env.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
		})
		if err != nil {
			return Fail("HeadObject: " + err.Error())
		}
		if head.ObjectLockLegalHoldStatus != want {
			return Fail(fmt.Sprintf("HeadObject x-amz-object-lock-legal-hold %q, want %s", head.ObjectLockLegalHoldStatus, want))
		}
	}
	return Pass()
}

// A version under a legal hold cannot be deleted, with the governance bypass
// or without it. Once the hold is off, it can.
func testDeleteObjectLegalHold(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-legal-hold-delete"
	defer lockCleanup(ctx, env, key)
	v, err := lockPut(ctx, env, key)
	if err != nil {
		return Error("setup: " + err.Error())
	}
	if err := setLegalHold(ctx, env, key, v, types.ObjectLockLegalHoldStatusOn); err != nil {
		return Error("setup: PutObjectLegalHold: " + err.Error())
	}
	for _, bypass := range []bool{false, true} {
		in := &s3.DeleteObjectInput{Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v)}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		_, err := env.Client.DeleteObject(ctx, in)
		what := fmt.Sprintf("DeleteObject of a version under a legal hold (bypass=%v)", bypass)
		if r := expectFailure(err, what, 403, "AccessDenied"); r.Status != StatusPass {
			return r
		}
	}
	if ok, err := versionExists(ctx, env, key, v); err != nil || !ok {
		return Fail(fmt.Sprintf("the held version is not readable after the refused deletes (exists=%v, err=%v)", ok, err))
	}
	if err := setLegalHold(ctx, env, key, v, types.ObjectLockLegalHoldStatusOff); err != nil {
		return Fail("PutObjectLegalHold OFF: " + err.Error())
	}
	if _, err := env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
	}); err != nil {
		return Fail("DeleteObject after the hold was turned off: " + err.Error())
	}
	if ok, err := versionExists(ctx, env, key, v); err != nil || ok {
		return Fail(fmt.Sprintf("the version was not deleted after the hold was turned off (exists=%v, err=%v)", ok, err))
	}
	return Pass()
}

// A default retention set with PutObjectLockConfiguration is read back and is
// applied to a version written without lock headers. The test removes the
// default again before it returns.
func testPutObjectLockConfigurationDefaultRetention(ctx context.Context, env *Env) TestResult {
	if r := needLock(env); r != nil {
		return *r
	}
	key := "lock-default-retention"
	defer lockCleanup(ctx, env, key)
	put := func(rule *types.ObjectLockRule) error {
		_, err := env.Client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
			Bucket: aws.String(env.LockBucket),
			ObjectLockConfiguration: &types.ObjectLockConfiguration{
				ObjectLockEnabled: types.ObjectLockEnabledEnabled, Rule: rule,
			},
		})
		return err
	}
	if err := put(&types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{
		Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1),
	}}); err != nil {
		return Fail("PutObjectLockConfiguration: " + err.Error())
	}
	defer func() { _ = put(nil) }()

	r := eventually(ctx, propagation, func() TestResult {
		out, err := env.Client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(env.LockBucket)})
		if err != nil {
			return Fail("GetObjectLockConfiguration: " + err.Error())
		}
		cfg := out.ObjectLockConfiguration
		if cfg == nil || cfg.Rule == nil || cfg.Rule.DefaultRetention == nil {
			return Fail("no default retention read back")
		}
		d := cfg.Rule.DefaultRetention
		if d.Mode != types.ObjectLockRetentionModeGovernance || aws.ToInt32(d.Days) != 1 || d.Years != nil {
			return Fail(fmt.Sprintf("default retention read back as mode=%q days=%d years=%v", d.Mode, aws.ToInt32(d.Days), d.Years))
		}
		return Pass()
	})
	if r.Status != StatusPass {
		return r
	}

	// The default applies to writes made after it propagated, so write until a
	// version carries it.
	return eventually(ctx, propagation, func() TestResult {
		start := time.Now()
		v, err := lockPut(ctx, env, key)
		if err != nil {
			return Fail("PutObject: " + err.Error())
		}
		out, err := env.Client.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{
			Bucket: aws.String(env.LockBucket), Key: aws.String(key), VersionId: aws.String(v),
		})
		if err != nil {
			return Fail("GetObjectRetention of a version written under a default retention: " + err.Error())
		}
		if out.Retention == nil || out.Retention.Mode != types.ObjectLockRetentionModeGovernance {
			return Fail("the version written under a default retention has no GOVERNANCE retention")
		}
		until := aws.ToTime(out.Retention.RetainUntilDate)
		lo, hi := start.Add(24*time.Hour-time.Minute), time.Now().Add(24*time.Hour+time.Minute)
		if until.Before(lo) || until.After(hi) {
			return Fail(fmt.Sprintf("retain until %s, want one day after the write", until.UTC().Format(time.RFC3339)))
		}
		return Pass()
	})
}
