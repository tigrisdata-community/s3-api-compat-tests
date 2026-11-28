package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/tigrisdata-community/s3-api-compat-tests/tests"
)

func main() {
	providers := flag.String("providers", "", "comma-separated provider names (default: all configured)")
	output := flag.String("output", "results.json", "path for JSON report")
	only := flag.String("tests", "", "comma-separated test names to run, in suite order (default: all)")
	flag.Parse()

	var names []string
	if *providers != "" {
		names = strings.Split(*providers, ",")
	}
	if *only != "" {
		onlyTests = map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			onlyTests[strings.TrimSpace(n)] = true
		}
	}

	providerList := LoadProviders(names)
	if len(providerList) == 0 {
		log.Fatal("no providers configured — set env vars (see .env.example)")
	}

	ctx := context.Background()
	report := Report{GeneratedAt: time.Now().UTC()}

	for _, p := range providerList {
		fmt.Printf("\n=== %s ===\n", p.Name)
		pr := runProvider(ctx, &p)
		report.Providers = append(report.Providers, pr)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(*output, data, 0644); err != nil {
		log.Fatalf("write report: %v", err)
	}
	fmt.Printf("\nReport written to %s\n", *output)

	// Print summary table.
	for _, pr := range report.Providers {
		fmt.Printf("\n%s: %d passed, %d failed, %d skipped, %d errors (of %d)\n",
			pr.Name, pr.Passed, pr.Failed, pr.Skipped, pr.Errors, pr.Total)
	}
}

// onlyTests, when set by --tests, limits the run to the named tests.
var onlyTests map[string]bool

func runProvider(ctx context.Context, p *Provider) ProviderReport {
	pr := ProviderReport{
		Name:     p.Name,
		Endpoint: p.Endpoint,
		Region:   p.Region,
	}

	client, err := p.NewClient(ctx)
	if err != nil {
		log.Printf("[%s] failed to create client: %v", p.Name, err)
		return pr
	}

	presignClient, err := p.NewPresignClient(ctx)
	if err != nil {
		log.Printf("[%s] failed to create presign client: %v", p.Name, err)
		return pr
	}

	bucket := fmt.Sprintf("s3compat-%s-%s", p.Name, randomSuffix())
	fmt.Printf("Using bucket: %s\n", bucket)

	// Create the test bucket.
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		log.Printf("[%s] failed to create bucket %s: %v", p.Name, bucket, err)
		return pr
	}
	defer cleanupBucket(ctx, client, bucket)

	env := &tests.Env{
		Client:        client,
		PresignClient: presignClient,
		Bucket:        bucket,
		Provider:      p.Name,
		Region:        p.Region,
	}

	// A second bucket with versioning on, for the versioning category. The
	// provider decides how it is created; when that fails the category skips.
	versioned := bucket + "-v"
	if err := p.CreateVersionedBucket(ctx, client, versioned); err != nil {
		fmt.Printf("No versioned bucket (%s): versioning tests will skip\n", err)
		env.VersionedBucketErr = err.Error()
		_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(versioned)})
	} else {
		fmt.Printf("Using versioned bucket: %s\n", versioned)
		env.VersionedBucket = versioned
		defer cleanupBucket(ctx, client, versioned)
	}

	// A third bucket with Object Lock on, for the object-lock category.
	locked := bucket + "-l"
	if err := p.CreateLockBucket(ctx, client, locked); err != nil {
		fmt.Printf("No object lock bucket (%s): object lock tests will skip\n", err)
		env.LockBucketErr = err.Error()
		_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(locked)})
	} else {
		fmt.Printf("Using object lock bucket: %s\n", locked)
		env.LockBucket = locked
		defer cleanupBucket(ctx, client, locked)
		// Runs first: a version a failed test left locked would keep the
		// bucket from being deleted.
		defer releaseLockedVersions(ctx, client, locked)
	}

	allTests := tests.All()
	for _, t := range allTests {
		if onlyTests != nil && !onlyTests[t.Name] {
			continue
		}
		start := time.Now()
		result := t.Fn(ctx, env)
		result.Operation = t.Name
		result.Category = t.Category
		result.DurationMs = time.Since(start).Milliseconds()

		symbol := "✓"
		switch result.Status {
		case tests.StatusFail:
			symbol = "✗"
		case tests.StatusSkip:
			symbol = "⊘"
		case tests.StatusError:
			symbol = "!"
		}
		msg := result.Operation
		if result.Error != "" {
			msg += ": " + result.Error
		} else if result.Notes != "" {
			msg += " (" + result.Notes + ")"
		}
		fmt.Printf("  %s %s\n", symbol, msg)

		pr.Results = append(pr.Results, result)
	}

	pr.Tally()
	return pr
}

// releaseLockedVersions deletes every version of an object lock bucket that
// the tests left behind: it turns a legal hold off and deletes with the
// governance bypass. The tests only ever set GOVERNANCE retention, which the
// bucket owner may bypass.
func releaseLockedVersions(ctx context.Context, client *s3.Client, bucket string) {
	paginator := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return
		}
		for _, v := range page.Versions {
			_, _ = client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
				Bucket: aws.String(bucket), Key: v.Key, VersionId: v.VersionId,
				LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOff},
			})
			_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(bucket), Key: v.Key, VersionId: v.VersionId,
				BypassGovernanceRetention: aws.Bool(true),
			})
		}
	}
}

func cleanupBucket(ctx context.Context, client *s3.Client, bucket string) {
	// Delete all object versions and delete markers first. Versioned buckets
	// will not delete unless every version is removed by VersionId. Providers
	// that do not support versioning return an empty list here.
	versionsPaginator := s3.NewListObjectVersionsPaginator(client, &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket),
	})
	for versionsPaginator.HasMorePages() {
		page, err := versionsPaginator.NextPage(ctx)
		if err != nil {
			break
		}
		var objects []types.ObjectIdentifier
		for _, v := range page.Versions {
			objects = append(objects, types.ObjectIdentifier{
				Key:       v.Key,
				VersionId: v.VersionId,
			})
		}
		for _, m := range page.DeleteMarkers {
			objects = append(objects, types.ObjectIdentifier{
				Key:       m.Key,
				VersionId: m.VersionId,
			})
		}
		if len(objects) == 0 {
			continue
		}
		_, _ = client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
	}

	// Delete remaining current objects (covers providers that don't expose
	// ListObjectVersions, and any objects the version-aware sweep missed).
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			break
		}
		if len(page.Contents) == 0 {
			continue
		}
		objects := make([]types.ObjectIdentifier, len(page.Contents))
		for i, obj := range page.Contents {
			objects[i] = types.ObjectIdentifier{Key: obj.Key}
		}
		_, _ = client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
	}

	// Abort any in-progress multipart uploads.
	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(bucket),
	})
	if err == nil {
		for _, u := range uploads.Uploads {
			_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(bucket),
				Key:      u.Key,
				UploadId: u.UploadId,
			})
		}
	}

	// Delete the bucket.
	_, _ = client.DeleteBucket(ctx, &s3.DeleteBucketInput{
		Bucket: aws.String(bucket),
	})
	fmt.Printf("Cleaned up bucket: %s\n", bucket)
}

func randomSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}
