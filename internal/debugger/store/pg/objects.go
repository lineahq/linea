package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// ErrObjectNotFound is returned by ObjectStore.Get for a missing key.
var ErrObjectNotFound = errors.New("object not found")

// ErrObjectChanged is returned by ObjectStore.Copy when the source is not the
// object the caller read.
var ErrObjectChanged = errors.New("object changed")

// ObjectReader is a streaming read of one object.
type ObjectReader struct {
	Body io.ReadCloser
	// Size is the object's length in bytes.
	Size int64
	// ETag identifies the content read, for ObjectStore.Copy.
	ETag string
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

// ObjectStore is the object storage the repository writes payloads to. Keys
// are opaque to it; the repository derives them from content digests.
type ObjectStore interface {
	// Put stores data under key, replacing any object there.
	Put(ctx context.Context, key string, data []byte) error
	// Get returns the object, or an error wrapping ErrObjectNotFound. It fails
	// if the object is longer than limit bytes.
	Get(ctx context.Context, key string, limit int64) ([]byte, error)
	// Open starts a streaming read of the object, or fails with an error
	// wrapping ErrObjectNotFound. The caller closes the body.
	Open(ctx context.Context, key string) (ObjectReader, error)
	// Copy copies src to dst inside the store, replacing any object at dst,
	// without moving the bytes through the caller. A nonempty ifMatch is the
	// ETag Open reported for src: the copy then fails with ErrObjectChanged
	// when src no longer has it, and with ErrObjectNotFound when src is gone.
	Copy(ctx context.Context, src, dst, ifMatch string) error
	// Delete removes an object; deleting a missing one is not an error.
	Delete(ctx context.Context, key string) error
	// List calls fn for every object whose key starts with prefix, stopping at
	// the first error fn returns.
	List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error
	// PresignGet and PresignPut return URLs that read or write one key
	// without credentials until the lifetime ends.
	PresignGet(ctx context.Context, key string, lifetime time.Duration) (string, error)
	PresignPut(ctx context.Context, key string, lifetime time.Duration) (string, error)
}

func cleanPrefix(p string) string { return strings.Trim(p, "/") }

// objectKey is <prefix>/sha256/<hex> for a "sha256:<hex>" digest.
func objectKey(prefix, digest string) (string, error) {
	hexSum, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || !isLowerHex(hexSum, 64) {
		return "", fmt.Errorf("%w: invalid digest", artifact.ErrCorrupt)
	}
	if prefix == "" {
		return "sha256/" + hexSum, nil
	}
	return prefix + "/sha256/" + hexSum, nil
}

// objectPrefix is the key prefix every object of the installation shares.
func objectPrefix(prefix string) string {
	if prefix == "" {
		return "sha256/"
	}
	return prefix + "/sha256/"
}

// putBlob stores raw under its digest. It always writes, which also refreshes
// the object's age.
func (s *Store) putBlob(ctx context.Context, digest string, raw []byte) error {
	key, err := objectKey(s.prefix, digest)
	if err != nil {
		return err
	}
	if err := s.objects.Put(ctx, key, raw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: store object: %w", artifact.ErrStorage, err)
	}
	return nil
}

// readBlob returns the object a row names, checking its size and digest.
func (s *Store) readBlob(ctx context.Context, digest string, size int64) ([]byte, error) {
	key, err := objectKey(s.prefix, digest)
	if err != nil {
		return nil, err
	}
	raw, err := s.objects.Get(ctx, key, size)
	switch {
	case errors.Is(err, ErrObjectNotFound):
		return nil, fmt.Errorf("%w: object unavailable", artifact.ErrCorrupt)
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("%w: read object: %w", artifact.ErrStorage, err)
	}
	if int64(len(raw)) != size || digestOf(raw) != digest {
		return nil, fmt.Errorf("%w: object changed", artifact.ErrCorrupt)
	}
	return raw, nil
}

// s3Store is the ObjectStore over any S3-compatible service.
type s3Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// newS3 builds the client and checks that the bucket exists. Checksums are
// calculated and validated only where S3 requires them: many S3-compatible
// servers reject or mishandle the trailing checksums the SDK adds by default.
func newS3Client(c S3Config) (*s3.Client, error) {
	if c.Bucket == "" {
		return nil, invalid("object storage bucket is required")
	}
	opts := s3.Options{
		Region:                     c.Region,
		Credentials:                credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		UsePathStyle:               c.UsePathStyle,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	if c.Endpoint != "" {
		endpoint := c.Endpoint
		if !strings.Contains(endpoint, "://") {
			scheme := "http://"
			if c.UseTLS {
				scheme = "https://"
			}
			endpoint = scheme + endpoint
		}
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return s3.New(opts), nil
}

func newS3(ctx context.Context, c S3Config) (ObjectStore, error) {
	client, err := newS3Client(c)
	if err != nil {
		return nil, err
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.Bucket)}); err != nil {
		return nil, fmt.Errorf("%w: object storage bucket %q: %w", artifact.ErrStorage, c.Bucket, err)
	}
	return &s3Store{client: client, presign: s3.NewPresignClient(client), bucket: c.Bucket}, nil
}

func (m *s3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := m.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(m.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String("application/octet-stream"),
	})
	return err
}

func (m *s3Store) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	out, err := m.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, notFound(err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("object %s is longer than %d bytes", key, limit)
	}
	return raw, nil
}

func (m *s3Store) Open(ctx context.Context, key string) (ObjectReader, error) {
	out, err := m.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectReader{}, notFound(err)
	}
	return ObjectReader{Body: out.Body, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag)}, nil
}

func (m *s3Store) Copy(ctx context.Context, src, dst, ifMatch string) error {
	in := &s3.CopyObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(dst), CopySource: aws.String(m.bucket + "/" + escapeKey(src))}
	if ifMatch != "" {
		in.CopySourceIfMatch = aws.String(ifMatch)
	}
	_, err := m.client.CopyObject(ctx, in)
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
		return ErrObjectChanged
	}
	return notFound(err)
}

// escapeKey percent-encodes a key for the x-amz-copy-source header, keeping
// its slashes.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func notFound(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
		return ErrObjectNotFound
	}
	return err
}

func (m *s3Store) Delete(ctx context.Context, key string) error {
	_, err := m.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	return err
}

func (m *s3Store) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	pages := s3.NewListObjectsV2Paginator(m.client, &s3.ListObjectsV2Input{Bucket: aws.String(m.bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			info := ObjectInfo{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				info.Modified = *o.LastModified
			}
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *s3Store) PresignGet(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	r, err := m.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)}, s3.WithPresignExpires(lifetime))
	if err != nil {
		return "", err
	}
	return r.URL, nil
}

func (m *s3Store) PresignPut(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	r, err := m.presign.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)}, s3.WithPresignExpires(lifetime))
	if err != nil {
		return "", err
	}
	return r.URL, nil
}
