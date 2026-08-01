package main

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Uploader puts a photo into the bucket the site reads from at build time.
type Uploader interface {
	Upload(ctx context.Context, key, contentType string, body []byte) error
}

type bucketUploader struct {
	client *s3.Client
	bucket string
}

// newBucketUploader reads credentials the usual AWS way, which on Fly means the
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY secrets Tigris issues. Note these
// are *write* credentials held by the server — the public read access the site
// build relies on needs no credentials at all.
func newBucketUploader(ctx context.Context, bucket, endpoint string) (*bucketUploader, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading bucket credentials: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	return &bucketUploader{client: client, bucket: bucket}, nil
}

func (u *bucketUploader) Upload(ctx context.Context, key, contentType string, body []byte) error {
	_, err := u.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(u.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
		// Photos are content-addressed, so the bytes behind a key never change.
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return fmt.Errorf("uploading %s: %w", key, err)
	}
	return nil
}
