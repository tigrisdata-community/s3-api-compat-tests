package tests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithy "github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Env holds the shared state for all tests against a single provider.
type Env struct {
	Client        *s3.Client
	PresignClient *s3.PresignClient
	Bucket        string
	Provider      string
	Region        string

	// VersionedBucket is a second bucket with object versioning enabled, created
	// the way the provider supports (PutBucketVersioning on AWS, a creation
	// header on Tigris). Empty when the provider could not create one; the
	// versioning tests then skip and VersionedBucketErr says why.
	VersionedBucket    string
	VersionedBucketErr string

	// LockBucket is a third bucket created with S3 Object Lock enabled
	// (CreateBucket with x-amz-bucket-object-lock-enabled). Empty when the
	// provider could not create one; the object lock tests then skip and
	// LockBucketErr says why.
	LockBucket    string
	LockBucketErr string
}

// TestDef defines a single test to run.
type TestDef struct {
	Name     string
	Category string
	Fn       TestFunc
}

// TestResult records the outcome of a test. Operation, Category, and DurationMs
// are filled in by the runner; the test function sets Status, Error, and Notes.
type TestResult struct {
	Operation  string `json:"operation"`
	Category   string `json:"category"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Notes      string `json:"notes,omitempty"`

	// final marks a verdict that a retry cannot change, such as "the bucket
	// itself was deleted"; eventually returns it at once.
	final bool
}

const (
	StatusPass  = "pass"
	StatusFail  = "fail"
	StatusSkip  = "skip"
	StatusError = "error"
)

func Pass() TestResult                    { return TestResult{Status: StatusPass} }
func Fail(err string) TestResult          { return TestResult{Status: StatusFail, Error: err} }
func FailFinal(err string) TestResult     { return TestResult{Status: StatusFail, Error: err, final: true} }
func Skip(reason string) TestResult       { return TestResult{Status: StatusSkip, Notes: reason} }
func Error(err string) TestResult         { return TestResult{Status: StatusError, Error: err} }
func PassWithNote(note string) TestResult { return TestResult{Status: StatusPass, Notes: note} }

// eventually retries fn until it passes or d elapses, and returns the last
// result. S3 documents bucket configuration changes as eventually consistent
// ("changes may take time to propagate"), so a configuration round trip reads
// back with a short wait instead of once.
func eventually(ctx context.Context, d time.Duration, fn func() TestResult) TestResult {
	deadline := time.Now().Add(d)
	for {
		r := fn()
		if r.Status == StatusPass || r.final || time.Now().After(deadline) {
			return r
		}
		select {
		case <-ctx.Done():
			return r
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// propagation is how long a bucket configuration read may lag its write.
const propagation = 15 * time.Second

// --- error inspection ---

// errCode returns the S3 error code of a failed call ("NoSuchKey",
// "PreconditionFailed", ...), or "" when the error carries none.
func errCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// httpStatus returns the HTTP status of a failed call, or 0 when unknown.
func httpStatus(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// expectFailure passes when err is a service error with the given HTTP status,
// or with one of the given S3 error codes. It fails when the call succeeded or
// failed differently. what names the call for the message.
func expectFailure(err error, what string, status int, codes ...string) TestResult {
	if err == nil {
		return Fail(fmt.Sprintf("%s: expected %s, got success", what, describeExpected(status, codes)))
	}
	code, got := errCode(err), httpStatus(err)
	if status != 0 && got == status {
		return Pass()
	}
	for _, c := range codes {
		if strings.EqualFold(code, c) {
			return Pass()
		}
	}
	return Fail(fmt.Sprintf("%s: expected %s, got HTTP %d %s: %v", what, describeExpected(status, codes), got, code, err))
}

func describeExpected(status int, codes []string) string {
	parts := []string{}
	if status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", status))
	}
	if len(codes) > 0 {
		parts = append(parts, strings.Join(codes, "/"))
	}
	return strings.Join(parts, " or ")
}
