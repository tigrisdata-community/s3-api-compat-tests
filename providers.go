package main

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Provider describes one S3-compatible service to test against.
type Provider struct {
	Name      string
	Endpoint  string // empty means default AWS endpoint
	Region    string
	PathStyle bool
	AccessKey string
	SecretKey string

	// StripSDKHeaders removes amz-sdk-* and accept-encoding headers before
	// signing. GCS rejects requests signed with headers it doesn't understand.
	StripSDKHeaders bool

	// ChecksumsOnlyWhenRequired makes the SDK add a request checksum only for
	// operations that require one, instead of on every PUT. GCS rejects the
	// signature of a request that carries the SDK's default CRC32 checksum.
	ChecksumsOnlyWhenRequired bool

	// VersionedBucketHeaders are sent with the CreateBucket request for the
	// versioned test bucket, for providers where versioning is a creation-time
	// property. When empty, the runner enables versioning with
	// PutBucketVersioning after creating the bucket.
	VersionedBucketHeaders map[string]string
}

// stripSDKHeadersMiddleware removes SDK-specific headers before signing.
// The retry handler adds Amz-Sdk-* headers in the Finalize step, so this
// middleware must also run in Finalize, before the signer.
type stripSDKHeadersMiddleware struct{}

func (stripSDKHeadersMiddleware) ID() string { return "StripSDKHeaders" }

func (stripSDKHeadersMiddleware) HandleFinalize(
	ctx context.Context,
	in middleware.FinalizeInput,
	next middleware.FinalizeHandler,
) (middleware.FinalizeOutput, middleware.Metadata, error) {
	req, ok := in.Request.(*smithyhttp.Request)
	if ok {
		req.Header.Del("Accept-Encoding")
		req.Header.Del("Amz-Sdk-Invocation-Id")
		req.Header.Del("Amz-Sdk-Request")
		// The SDK adds a CRC32 checksum to operations that require one; GCS
		// does not know that header and wants Content-MD5 instead, so compute
		// the MD5 from the body and drop the SDK's default. A checksum a test
		// sets on purpose (SHA-256) is left as sent.
		hadDefault := false
		if req.Header.Get("X-Amz-Checksum-Crc32") != "" {
			req.Header.Del("X-Amz-Checksum-Crc32")
			hadDefault = true
			if strings.EqualFold(req.Header.Get("X-Amz-Sdk-Checksum-Algorithm"), "CRC32") {
				req.Header.Del("X-Amz-Sdk-Checksum-Algorithm")
			}
		}
		if hadDefault && req.Header.Get("Content-Md5") == "" {
			if stream := req.GetStream(); stream != nil {
				if body, err := io.ReadAll(stream); err == nil {
					sum := md5.Sum(body)
					req.Header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum[:]))
					_ = req.RewindStream()
				}
			}
		}
	}
	return next.HandleFinalize(ctx, in)
}

// NewClient builds an s3.Client for this provider.
func (p *Provider) NewClient(ctx context.Context) (*s3.Client, error) {
	creds := credentials.NewStaticCredentialsProvider(p.AccessKey, p.SecretKey, "")
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(creds),
		config.WithRegion(p.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("load config for %s: %w", p.Name, err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if p.Endpoint != "" {
			o.BaseEndpoint = aws.String(p.Endpoint)
		}
		o.UsePathStyle = p.PathStyle
		o.Region = p.Region
		o.DisableLogOutputChecksumValidationSkipped = true
		if p.ChecksumsOnlyWhenRequired {
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
		if p.StripSDKHeaders {
			o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
				// Try to insert before "Signing" (normal client) or just add
				// at the start of Finalize (presign client has no Signing).
				if err := stack.Finalize.Insert(stripSDKHeadersMiddleware{}, "Signing", middleware.Before); err != nil {
					return stack.Finalize.Add(stripSDKHeadersMiddleware{}, middleware.Before)
				}
				return nil
			})
		}
	}), nil
}

// NewPresignClient builds a presign client for this provider.
func (p *Provider) NewPresignClient(ctx context.Context) (*s3.PresignClient, error) {
	client, err := p.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return s3.NewPresignClient(client), nil
}

// CreateVersionedBucket creates a bucket with object versioning enabled, the
// way this provider supports it, and returns an error that says why when it
// cannot.
func (p *Provider) CreateVersionedBucket(ctx context.Context, client *s3.Client, name string) error {
	var opts []func(*s3.Options)
	for k, v := range p.VersionedBucketHeaders {
		k, v := k, v
		opts = append(opts, func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, smithyhttp.AddHeaderValue(k, v))
		})
	}
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}, opts...); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	if len(p.VersionedBucketHeaders) == 0 {
		if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: aws.String(name),
			VersioningConfiguration: &types.VersioningConfiguration{
				Status: types.BucketVersioningStatusEnabled,
			},
		}); err != nil {
			return fmt.Errorf("enable versioning: %w", err)
		}
	}
	// Bucket settings are eventually consistent on S3, so read the status
	// back for a while before deciding the bucket has no versioning.
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(name)})
		if err == nil && out.Status == types.BucketVersioningStatusEnabled {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("get versioning: %w", err)
			}
			return fmt.Errorf("versioning status is %q after enabling", out.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// LoadProviders reads env vars and returns providers that have credentials configured.
// If names is non-empty, only those named providers are returned.
func LoadProviders(names []string) []Provider {
	all := []Provider{
		{
			Name:      "tigris",
			Endpoint:  "https://t3.storage.dev",
			Region:    "auto",
			PathStyle: false,
			AccessKey: firstEnv("TIGRIS_ACCESS_KEY_ID", "TIGRIS_STORAGE_ACCESS_KEY_ID"),
			SecretKey: firstEnv("TIGRIS_SECRET_ACCESS_KEY", "TIGRIS_STORAGE_SECRET_ACCESS_KEY"),
			// Versioning is a creation-time property on Tigris.
			VersionedBucketHeaders: map[string]string{"X-Tigris-Enable-Snapshot": "true"},
		},
		{
			Name:      "aws",
			Endpoint:  "",
			Region:    envOr("AWS_S3_REGION", "us-east-1"),
			PathStyle: false,
			AccessKey: os.Getenv("AWS_S3_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("AWS_S3_SECRET_ACCESS_KEY"),
		},
		{
			Name:      "r2",
			Endpoint:  firstEnv("R2_ENDPOINT", "CLOUDFLARE_ENDPOINT"),
			Region:    "auto",
			PathStyle: false,
			AccessKey: firstEnv("R2_ACCESS_KEY_ID", "CLOUDFLARE_ACCESS_KEY_ID"),
			SecretKey: firstEnv("R2_SECRET_ACCESS_KEY", "CLOUDFLARE_SECRET_ACCESS_KEY"),
		},
		{
			Name:            "gcs",
			Endpoint:        "https://storage.googleapis.com",
			Region:          "auto",
			PathStyle:       true,
			AccessKey:       os.Getenv("GCS_ACCESS_KEY_ID"),
			SecretKey:       os.Getenv("GCS_SECRET_ACCESS_KEY"),
			StripSDKHeaders: true,

			ChecksumsOnlyWhenRequired: true,
		},
		{
			// Neon Object Storage: each Neon branch has its own S3 endpoint
			// (https://<branch-id>.storage.<cell>.<region>.aws.neon.tech) and
			// only path-style addressing is supported. The access key is the
			// credential's token_id (nak_live_...) and the secret is its
			// s3_secret_access_key (nsk_live_...).
			Name:      "neon",
			Endpoint:  os.Getenv("NEON_STORAGE_ENDPOINT"),
			Region:    envOr("NEON_STORAGE_REGION", "us-east-2"),
			PathStyle: true,
			AccessKey: os.Getenv("NEON_STORAGE_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("NEON_STORAGE_SECRET_ACCESS_KEY"),
		},
	}

	// Filter to requested names, or return all configured.
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}

	var out []Provider
	for _, p := range all {
		if p.AccessKey == "" || p.SecretKey == "" {
			continue
		}
		if len(want) > 0 && !want[p.Name] {
			continue
		}
		// Only AWS has a default endpoint. Without one, the SDK would send
		// this provider's credentials to AWS S3.
		if p.Endpoint == "" && p.Name != "aws" {
			fmt.Fprintf(os.Stderr, "skipping %s: credentials set but no endpoint configured\n", p.Name)
			continue
		}
		out = append(out, p)
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
