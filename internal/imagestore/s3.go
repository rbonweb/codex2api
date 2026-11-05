package imagestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Backend adapts any S3-compatible object storage: AWS S3, Cloudflare R2, MinIO, Alibaba Cloud OSS,
// Tencent Cloud COS, Backblaze B2, Huawei OBS, Qiniu Kodo and others.
//
// The ref form is fixed as "s3://<bucket>/<key>". The bucket matches the configuration; the key includes the prefix.
type S3Backend struct {
	client *s3.Client
	bucket string
	prefix string
}

// NewS3Backend builds a client from cfg. When Region is empty it defaults to "auto" (compatible with R2).
//
// Compatibility handling:
//   - SwapComputePayloadSHA256ForUnsignedPayloadMiddleware: avoids computing SHA256 over the PUT body,
//     working around signing differences in services such as Alibaba Cloud OSS and Tencent Cloud COS.
//   - RequestChecksumCalculationWhenRequired: computes checksums only when the protocol requires them,
//     so the CRC64 that newer SDKs enable by default does not break older S3 implementations.
//   - UsePathStyle: required for MinIO and some self-hosted or early S3 services.
func NewS3Backend(cfg Config) (*S3Backend, error) {
	cfg = cfg.Normalize()
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("imagestore.s3: bucket is missing")
	}

	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})

	return &S3Backend{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// Name implements Backend.
func (b *S3Backend) Name() string { return BackendS3 }

// Save uploads one object. It returns a ref in the form "s3://bucket/key".
func (b *S3Backend) Save(ctx context.Context, key string, data []byte, mime string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("imagestore.s3: key is empty")
	}
	fullKey := b.prefix + key
	contentType := mime
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &b.bucket,
		Key:         &fullKey,
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return "", fmt.Errorf("S3 PutObject: %w", err)
	}
	return buildS3Ref(b.bucket, fullKey), nil
}

// Open fetches the object content as a stream. size comes from Content-Length when available.
func (b *S3Backend) Open(ctx context.Context, ref string) (io.ReadCloser, int64, error) {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return nil, 0, err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("S3 GetObject: %w", err)
	}
	var size int64 = -1
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

// Read reads all content at once.
func (b *S3Backend) Read(ctx context.Context, ref string) ([]byte, error) {
	rc, _, err := b.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Delete deletes the object.
func (b *S3Backend) Delete(ctx context.Context, ref string) error {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	_, err = b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("S3 DeleteObject: %w", err)
	}
	return nil
}

// HeadBucket provides a liveness check, used by the "test connection" button on the settings page.
func (b *S3Backend) HeadBucket(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &b.bucket})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket: %w", err)
	}
	return nil
}

// PresignGetURL generates a presigned GET direct link with an expiry time for one ref.
//
// The signature is computed locally without making a network request, so it can be unit-tested offline. It works with private buckets:
// whoever holds the link can GET the object directly within the ttl, without public read permission.
func (b *S3Backend) PresignGetURL(ctx context.Context, ref string, ttl time.Duration) (string, error) {
	bucket, key, err := parseS3Ref(ref)
	if err != nil {
		return "", err
	}
	if bucket == "" {
		bucket = b.bucket
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	presign := s3.NewPresignClient(b.client)
	out, err := presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("S3 PresignGetObject: %w", err)
	}
	return out.URL, nil
}

func buildS3Ref(bucket, key string) string {
	return s3RefScheme + bucket + "/" + key
}

func parseS3Ref(ref string) (bucket, key string, err error) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, s3RefScheme) {
		return "", "", fmt.Errorf("imagestore.s3: not an S3 ref: %s", ref)
	}
	rest := strings.TrimPrefix(ref, s3RefScheme)
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", fmt.Errorf("imagestore.s3: invalid ref: %s", ref)
	}
	return rest[:idx], rest[idx+1:], nil
}

// Compile-time interface check.
var _ Backend = (*S3Backend)(nil)

// SaveReader uploads an encoded image without allocating another image buffer.
func (b *S3Backend) SaveReader(ctx context.Context, key string, source io.ReadSeeker, size int64, mime string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("empty image key")
	}
	fullKey := b.prefix + key
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &b.bucket, Key: &fullKey, Body: source, ContentType: &mime, ContentLength: &size})
	if err != nil {
		return "", err
	}
	return buildS3Ref(b.bucket, fullKey), nil
}
