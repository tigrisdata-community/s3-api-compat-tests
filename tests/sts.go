package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// STSTests cover temporary credentials: STS GetFederationToken mints a session
// limited by an inline policy to the test bucket, and S3 must accept exactly
// what that policy names when a request is signed with the session and its
// token. They skip on a provider without an STS endpoint.
//
// Expiry is not covered: the shortest session STS issues lasts 15 minutes.
func STSTests() []TestDef {
	return []TestDef{
		{"GetCallerIdentity", "sts", testGetCallerIdentity},
		{"GetFederationToken", "sts", testGetFederationToken},
		{"GetFederationToken_InvalidDuration", "sts", testGetFederationTokenInvalidDuration},
		{"GetFederationToken_MalformedPolicy", "sts", testGetFederationTokenMalformedPolicy},
		{"Session_GetCallerIdentity", "sts", testSessionGetCallerIdentity},
		{"Session_PutObject", "sts", testSessionPutObject},
		{"Session_GetObject", "sts", testSessionGetObject},
		{"Session_ListObjectsV2", "sts", testSessionListObjectsV2},
		{"Session_DeleteObject", "sts", testSessionDeleteObject},
		{"Session_MultipartUpload", "sts", testSessionMultipartUpload},
		{"Session_PresignGetObject", "sts", testSessionPresignGetObject},
		{"Session_DeniedAction", "sts", testSessionDeniedAction},
		{"Session_DeniedBucket", "sts", testSessionDeniedBucket},
		{"Session_DeniedListBuckets", "sts", testSessionDeniedListBuckets},
		{"Session_MissingToken", "sts", testSessionMissingToken},
		{"Session_InvalidToken", "sts", testSessionInvalidToken},
		{"Session_CannotGetFederationToken", "sts", testSessionCannotGetFederationToken},
	}
}

// The shortest lifetime STS accepts.
const sessionSeconds = 900

// The two sessions the tests share, by federated user name.
const (
	sessionReadWrite = "s3compat-rw"
	sessionReadOnly  = "s3compat-ro"
)

var sessionActions = map[string][]string{
	sessionReadWrite: {
		"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket",
		"s3:AbortMultipartUpload", "s3:ListMultipartUploadParts",
	},
	sessionReadOnly: {"s3:GetObject", "s3:ListBucket"},
}

func needSTS(env *Env) *TestResult {
	if env.STS == nil {
		r := Skip("no STS: " + env.STSErr)
		return &r
	}
	return nil
}

// sessionPolicy allows the actions on the bucket and its objects.
func sessionPolicy(bucket string, actions []string) string {
	return fmt.Sprintf(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["%s"],"Resource":["arn:aws:s3:::%s","arn:aws:s3:::%s/*"]}]}`,
		strings.Join(actions, `","`), bucket, bucket)
}

func federationToken(ctx context.Context, env *Env, name string, actions []string) (*sts.GetFederationTokenOutput, error) {
	return env.STS.GetFederationToken(ctx, &sts.GetFederationTokenInput{
		Name:            aws.String(name),
		Policy:          aws.String(sessionPolicy(env.Bucket, actions)),
		DurationSeconds: aws.Int32(sessionSeconds),
	})
}

// session returns the shared session of that name, minting it on first use.
func session(ctx context.Context, env *Env, name string) (*Session, error) {
	if s := env.sessions[name]; s != nil {
		return s, nil
	}
	out, err := federationToken(ctx, env, name, sessionActions[name])
	if err != nil {
		return nil, fmt.Errorf("GetFederationToken: %w", err)
	}
	if out.Credentials == nil {
		return nil, fmt.Errorf("GetFederationToken returned no credentials")
	}
	c := out.Credentials
	s, err := env.NewSession(aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken))
	if err != nil {
		return nil, err
	}
	s.AccessKey, s.SecretKey, s.Token = aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken)
	if env.sessions == nil {
		env.sessions = map[string]*Session{}
	}
	env.sessions[name] = s
	return s, nil
}

func readBody(ctx context.Context, client *s3.Client, bucket, key string) (string, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return "", err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	return string(b), err
}

// --- the STS API with the long-lived key ---

func testGetCallerIdentity(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	out, err := env.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Fail(err.Error())
	}
	if aws.ToString(out.Account) == "" || aws.ToString(out.UserId) == "" || !strings.HasPrefix(aws.ToString(out.Arn), "arn:") {
		return Fail(fmt.Sprintf("identity is incomplete: account %q, user id %q, arn %q",
			aws.ToString(out.Account), aws.ToString(out.UserId), aws.ToString(out.Arn)))
	}
	return Pass()
}

func testGetFederationToken(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	const name = "s3compat-mint"
	before := time.Now()
	out, err := federationToken(ctx, env, name, sessionActions[sessionReadOnly])
	if err != nil {
		return Fail(err.Error())
	}
	c := out.Credentials
	if c == nil || aws.ToString(c.AccessKeyId) == "" || aws.ToString(c.SecretAccessKey) == "" || aws.ToString(c.SessionToken) == "" {
		return Fail("credentials lack an access key id, a secret or a session token")
	}
	// The clock of the provider may differ from ours by a few minutes.
	want := before.Add(sessionSeconds * time.Second)
	if c.Expiration == nil || c.Expiration.Before(want.Add(-5*time.Minute)) || c.Expiration.After(want.Add(5*time.Minute)) {
		return Fail(fmt.Sprintf("Expiration %v is not %d seconds from now", c.Expiration, sessionSeconds))
	}
	fed := out.FederatedUser
	if fed == nil || !strings.HasSuffix(aws.ToString(fed.Arn), ":federated-user/"+name) || !strings.HasSuffix(aws.ToString(fed.FederatedUserId), ":"+name) {
		return Fail(fmt.Sprintf("FederatedUser does not name %q: %+v", name, fed))
	}
	return Pass()
}

func testGetFederationTokenInvalidDuration(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	_, err := env.STS.GetFederationToken(ctx, &sts.GetFederationTokenInput{
		Name:            aws.String("s3compat-short"),
		Policy:          aws.String(sessionPolicy(env.Bucket, sessionActions[sessionReadOnly])),
		DurationSeconds: aws.Int32(sessionSeconds - 1),
	})
	return expectFailure(err, "GetFederationToken with DurationSeconds below 900", 0, "ValidationError")
}

func testGetFederationTokenMalformedPolicy(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	_, err := env.STS.GetFederationToken(ctx, &sts.GetFederationTokenInput{
		Name:            aws.String("s3compat-policy"),
		Policy:          aws.String("not a policy document"),
		DurationSeconds: aws.Int32(sessionSeconds),
	})
	return expectFailure(err, "GetFederationToken with a policy that is not JSON", 0, "MalformedPolicyDocument")
}

// --- S3 and STS signed with a session ---

func testSessionGetCallerIdentity(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	out, err := s.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Fail(err.Error())
	}
	if !strings.HasSuffix(aws.ToString(out.Arn), ":federated-user/"+sessionReadWrite) {
		return Fail(fmt.Sprintf("Arn %q is not the federated user %q", aws.ToString(out.Arn), sessionReadWrite))
	}
	return Pass()
}

func testSessionPutObject(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	key, body := "sts-put", "written with a session"
	defer deleteObject(ctx, env, key)
	if _, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key), Body: strings.NewReader(body),
	}); err != nil {
		return Fail(err.Error())
	}
	// Read back with the long-lived key: the object is in the bucket.
	got, err := getObjectBody(ctx, env, key)
	if err != nil || got != body {
		return Fail(fmt.Sprintf("object not stored by the session: %q %v", got, err))
	}
	return Pass()
}

func testSessionGetObject(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadOnly)
	if err != nil {
		return Error(err.Error())
	}
	key, body := "sts-get", "read with a session"
	defer deleteObject(ctx, env, key)
	if _, err := putBody(ctx, env, env.Bucket, key, body); err != nil {
		return Error("put: " + err.Error())
	}
	got, err := readBody(ctx, s.Client, env.Bucket, key)
	if err != nil {
		return Fail(err.Error())
	}
	if got != body {
		return Fail(fmt.Sprintf("body is %q, want %q", got, body))
	}
	if _, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(env.Bucket), Key: aws.String(key)}); err != nil {
		return Fail("HeadObject: " + err.Error())
	}
	return Pass()
}

func testSessionListObjectsV2(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadOnly)
	if err != nil {
		return Error(err.Error())
	}
	prefix := "sts-list/"
	keys := []string{prefix + "a", prefix + "b"}
	for _, k := range keys {
		defer deleteObject(ctx, env, k)
		if _, err := putBody(ctx, env, env.Bucket, k, "x"); err != nil {
			return Error("put: " + err.Error())
		}
	}
	out, err := s.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(env.Bucket), Prefix: aws.String(prefix)})
	if err != nil {
		return Fail(err.Error())
	}
	var got []string
	for _, o := range out.Contents {
		got = append(got, aws.ToString(o.Key))
	}
	if strings.Join(got, ",") != strings.Join(keys, ",") {
		return Fail(fmt.Sprintf("listed %v, want %v", got, keys))
	}
	return Pass()
}

func testSessionDeleteObject(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	key := "sts-delete"
	defer deleteObject(ctx, env, key)
	if err := putTestObject(ctx, env, key); err != nil {
		return Error("put: " + err.Error())
	}
	if _, err := s.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(env.Bucket), Key: aws.String(key)}); err != nil {
		return Fail(err.Error())
	}
	_, err = headObject(ctx, env, key)
	return expectFailure(err, "HeadObject after the session deleted the object", http.StatusNotFound)
}

func testSessionMultipartUpload(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	key, body := "sts-multipart", strings.Repeat("m", 1024)
	defer deleteObject(ctx, env, key)
	create, err := s.Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(env.Bucket), Key: aws.String(key)})
	if err != nil {
		return Fail("CreateMultipartUpload: " + err.Error())
	}
	defer abortUpload(ctx, env, key, create.UploadId)
	part, err := s.Client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key), UploadId: create.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader(body),
	})
	if err != nil {
		return Fail("UploadPart: " + err.Error())
	}
	if _, err := s.Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key), UploadId: create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}},
		},
	}); err != nil {
		return Fail("CompleteMultipartUpload: " + err.Error())
	}
	got, err := getObjectBody(ctx, env, key)
	if err != nil || got != body {
		return Fail(fmt.Sprintf("completed object has %d bytes, want %d: %v", len(got), len(body), err))
	}
	return Pass()
}

func testSessionPresignGetObject(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadOnly)
	if err != nil {
		return Error(err.Error())
	}
	key, body := "sts-presign", "presigned with a session"
	defer deleteObject(ctx, env, key)
	if _, err := putBody(ctx, env, env.Bucket, key, body); err != nil {
		return Error("put: " + err.Error())
	}
	presigned, err := s.PresignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		return Fail(err.Error())
	}
	if !strings.Contains(presigned.URL, "X-Amz-Security-Token=") {
		return Fail("presigned URL carries no X-Amz-Security-Token")
	}
	resp, err := doPresigned(ctx, presignedRequest{presigned.URL, presigned.Method, presigned.SignedHeader}, nil, -1)
	if err != nil {
		return Fail("http: " + err.Error())
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != body {
		return Fail(fmt.Sprintf("http status %d, body %q", resp.StatusCode, got))
	}
	return Pass()
}

// --- what a session is refused ---

func testSessionDeniedAction(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadOnly)
	if err != nil {
		return Error(err.Error())
	}
	key := "sts-denied-put"
	defer deleteObject(ctx, env, key)
	_, err = s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(env.Bucket), Key: aws.String(key), Body: strings.NewReader("x"),
	})
	if r := expectFailure(err, "PutObject with a session whose policy has no s3:PutObject", 0, "AccessDenied"); r.Status != StatusPass {
		return r
	}
	_, err = headObject(ctx, env, key)
	return expectFailure(err, "HeadObject after the refused PutObject", http.StatusNotFound)
}

func testSessionDeniedBucket(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	// A bucket the policy does not name: the versioned bucket of the run, or
	// a plain one made here when the provider could not create that.
	other, key := env.VersionedBucket, "sts-other-bucket"
	if other == "" {
		other = env.Bucket + "-o"
		if _, err := env.Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(other)}); err != nil {
			return Error("create a second bucket: " + err.Error())
		}
		defer env.Client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(other)})
		defer env.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(other), Key: aws.String(key)})
	} else {
		defer deleteAllVersions(ctx, env, key)
	}
	if _, err := putBody(ctx, env, other, key, "x"); err != nil {
		return Error("put: " + err.Error())
	}
	_, err = readBody(ctx, s.Client, other, key)
	return expectFailure(err, "GetObject in a bucket the session policy does not name", 0, "AccessDenied")
}

func testSessionDeniedListBuckets(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	_, err = s.Client.ListBuckets(ctx, &s3.ListBucketsInput{})
	return expectFailure(err, "ListBuckets with a session limited to one bucket", 0, "AccessDenied")
}

// sessionWith returns S3 clients for the shared read-only session with its
// token replaced, and puts an object for them to ask for.
func sessionWith(ctx context.Context, env *Env, key string, token func(string) string) (*Session, error) {
	s, err := session(ctx, env, sessionReadOnly)
	if err != nil {
		return nil, err
	}
	if _, err := putBody(ctx, env, env.Bucket, key, "x"); err != nil {
		return nil, fmt.Errorf("put: %w", err)
	}
	return env.NewSession(s.AccessKey, s.SecretKey, token(s.Token))
}

func testSessionMissingToken(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	key := "sts-missing-token"
	defer deleteObject(ctx, env, key)
	s, err := sessionWith(ctx, env, key, func(string) string { return "" })
	if err != nil {
		return Error(err.Error())
	}
	_, err = readBody(ctx, s.Client, env.Bucket, key)
	return expectFailure(err, "GetObject signed with a session key and no token", 0, "InvalidAccessKeyId")
}

func testSessionInvalidToken(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	key := "sts-invalid-token"
	defer deleteObject(ctx, env, key)
	s, err := sessionWith(ctx, env, key, func(token string) string {
		end := "AAAAAA"
		if strings.HasSuffix(token, end) {
			end = "BBBBBB"
		}
		return token[:max(len(token)-len(end), 0)] + end
	})
	if err != nil {
		return Error(err.Error())
	}
	_, err = readBody(ctx, s.Client, env.Bucket, key)
	return expectFailure(err, "GetObject signed with a session whose token was altered", 0, "InvalidToken")
}

func testSessionCannotGetFederationToken(ctx context.Context, env *Env) TestResult {
	if r := needSTS(env); r != nil {
		return *r
	}
	s, err := session(ctx, env, sessionReadWrite)
	if err != nil {
		return Error(err.Error())
	}
	_, err = s.STS.GetFederationToken(ctx, &sts.GetFederationTokenInput{
		Name:            aws.String("s3compat-nested"),
		Policy:          aws.String(sessionPolicy(env.Bucket, sessionActions[sessionReadOnly])),
		DurationSeconds: aws.Int32(sessionSeconds),
	})
	return expectFailure(err, "GetFederationToken signed with a session", 0, "AccessDenied")
}
