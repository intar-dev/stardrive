package objectstorage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Client struct {
	s3       *s3.Client
	presign  *s3.PresignClient
	endpoint string
}

func New(endpoint, accessKey, secretKey string) (*Client, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("S3 endpoint is required")
	}

	cfg := aws.Config{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(strings.TrimSpace(accessKey), strings.TrimSpace(secretKey), ""),
		BaseEndpoint: aws.String(endpoint),
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	return &Client{
		s3:       client,
		presign:  s3.NewPresignClient(client),
		endpoint: endpoint,
	}, nil
}

func (c *Client) EnsureBucket(ctx context.Context, bucket string) error {
	bucket = strings.TrimSpace(bucket)
	slog.Debug("ensuring S3 bucket", "endpoint", c.endpoint, "bucket", bucket)
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return nil
	}

	_, err = c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "bucketalreadyownedbyyou") {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}

func (c *Client) UploadFile(ctx context.Context, bucket, key, path string) error {
	slog.Debug("uploading file to S3", "endpoint", c.endpoint, "bucket", strings.TrimSpace(bucket), "key", strings.Trim(strings.TrimSpace(key), "/"), "path", path)
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	if _, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(strings.TrimSpace(bucket)),
		Key:    aws.String(strings.Trim(strings.TrimSpace(key), "/")),
		Body:   file,
	}); err != nil {
		return fmt.Errorf("upload %s to s3://%s/%s: %w", path, bucket, key, err)
	}

	return nil
}

func (c *Client) PutObject(ctx context.Context, bucket, key string, body io.Reader) error {
	if _, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(strings.TrimSpace(bucket)),
		Key:    aws.String(strings.Trim(strings.TrimSpace(key), "/")),
		Body:   body,
	}); err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

func (c *Client) PresignGetObject(ctx context.Context, bucket, key string) (string, error) {
	out, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(strings.TrimSpace(bucket)),
		Key:    aws.String(strings.Trim(strings.TrimSpace(key), "/")),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = 24 * time.Hour
	})
	if err != nil {
		return "", fmt.Errorf("presign get object s3://%s/%s: %w", bucket, key, err)
	}
	return out.URL, nil
}

func (c *Client) ListBuckets(ctx context.Context) ([]string, error) {
	out, err := c.s3.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, fmt.Errorf("list buckets: %w", err)
	}

	buckets := make([]string, 0, len(out.Buckets))
	for _, bucket := range out.Buckets {
		if bucket.Name != nil && strings.TrimSpace(*bucket.Name) != "" {
			buckets = append(buckets, strings.TrimSpace(*bucket.Name))
		}
	}
	return buckets, nil
}

func (c *Client) SyncDirectory(ctx context.Context, bucket, prefix, root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("root directory is required")
	}
	slog.Debug("syncing directory to S3", "endpoint", c.endpoint, "bucket", strings.TrimSpace(bucket), "prefix", strings.Trim(strings.TrimSpace(prefix), "/"), "root", root)
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(filepath.Join(prefix, rel))
		return c.UploadFile(ctx, bucket, key, path)
	})
}

func (c *Client) DeletePrefix(ctx context.Context, bucket, prefix string) error {
	paginator := s3.NewListObjectsV2Paginator(c.s3, &s3.ListObjectsV2Input{
		Bucket: aws.String(strings.TrimSpace(bucket)),
		Prefix: aws.String(strings.Trim(strings.TrimSpace(prefix), "/")),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, item := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: item.Key})
		}
		if _, err := c.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(strings.TrimSpace(bucket)),
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		}); err != nil {
			return fmt.Errorf("delete objects in s3://%s/%s: %w", bucket, prefix, err)
		}
	}
	return nil
}

func (c *Client) DeleteBucket(ctx context.Context, bucket string) error {
	if _, err := c.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(strings.TrimSpace(bucket))}); err != nil {
		return fmt.Errorf("delete bucket %s: %w", bucket, err)
	}
	return nil
}
