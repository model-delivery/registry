// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the object-store backend. It works with any service
// that speaks the S3 API: AWS S3, MinIO, Cloudflare R2, Backblaze B2, Ceph.
type S3Config struct {
	// Endpoint is the service URL, for example https://s3.us-east-1.amazonaws.com
	// or http://minio:9000. The scheme decides whether TLS is used.
	Endpoint string
	Region   string
	Bucket   string
	// Prefix is prepended to every object key, so a bucket can be shared.
	Prefix string
	// AccessKey and SecretKey are static credentials. When both are empty
	// the credentials come from the environment the process runs in (an
	// instance or pod role).
	AccessKey    string
	SecretKey    string
	SessionToken string
	// StagingDir is local scratch space. An upload is received and verified
	// here before it is sent to the bucket, so it needs room for the
	// largest file being received.
	StagingDir string
}

// S3 stores blobs as objects named <prefix>sha256/<2 hex>/<digest>.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
	tmpDir string
}

// NewS3 connects to the bucket and checks that it exists and is reachable.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: a bucket name is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3.amazonaws.com"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("s3: endpoint must be a URL such as https://s3.amazonaws.com or http://minio:9000, got %q", cfg.Endpoint)
	}
	creds := credentials.NewIAM("")
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	}
	client, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", Region: cfg.Region})
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	if err := os.RemoveAll(cfg.StagingDir); err != nil {
		return nil, fmt.Errorf("clear staging dir: %w", err)
	}
	if err := os.MkdirAll(cfg.StagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	s := &S3{client: client, bucket: cfg.Bucket, prefix: prefix, tmpDir: cfg.StagingDir}

	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("s3: cannot reach bucket %q at %s: %w", cfg.Bucket, cfg.Endpoint, err)
	}
	if !ok {
		return nil, fmt.Errorf("s3: bucket %q does not exist at %s; create it first", cfg.Bucket, cfg.Endpoint)
	}
	return s, nil
}

func (s *S3) key(digest string) (string, error) {
	k, err := blobKey(digest)
	if err != nil {
		return "", err
	}
	return s.prefix + k, nil
}

func isNoSuchKey(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NotFound"
}

// Stage receives and verifies the bytes on local disk. Nothing is sent to
// the bucket until Publish, so an upload that fails verification never
// becomes an object.
func (s *S3) Stage(ctx context.Context, r io.Reader, want Expect) (Staged, error) {
	tmp, info, err := stageToFile(ctx, s.tmpDir, r, want)
	if err != nil {
		return nil, err
	}
	return &s3Staged{store: s, tmp: tmp, info: info}, nil
}

// Open returns the object. Reads and seeks become ranged requests, so a
// client asking for part of a file only pulls that part from the bucket.
func (s *S3) Open(ctx context.Context, digest string) (File, int64, error) {
	key, err := s.key(digest)
	if err != nil {
		return nil, 0, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("s3: open %s: %w", digest, err)
	}
	st, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		if isNoSuchKey(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
		}
		return nil, 0, fmt.Errorf("s3: stat %s: %w", digest, err)
	}
	return obj, st.Size, nil
}

// Delete removes the object. Deleting an absent object is not an error.
func (s *S3) Delete(ctx context.Context, digest string) error {
	key, err := s.key(digest)
	if err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return fmt.Errorf("s3: delete %s: %w", digest, err)
	}
	return nil
}

// PresignGet returns a URL that downloads the object without credentials
// until ttl passes.
func (s *S3) PresignGet(ctx context.Context, digest string, ttl time.Duration) (string, error) {
	key, err := s.key(digest)
	if err != nil {
		return "", err
	}
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", fmt.Errorf("s3: presign %s: %w", digest, err)
	}
	return u.String(), nil
}

type s3Staged struct {
	store *S3
	tmp   string
	info  Info
	done  bool
}

func (st *s3Staged) Info() Info { return st.info }

func (st *s3Staged) Publish(ctx context.Context) error {
	if st.done {
		return nil
	}
	key, err := st.store.key(st.info.SHA256)
	if err != nil {
		return err
	}
	// Same digest and size means the object is already there.
	if cur, err := st.store.client.StatObject(ctx, st.store.bucket, key, minio.StatObjectOptions{}); err == nil && cur.Size == st.info.Size {
		st.Discard()
		return nil
	}
	f, err := os.Open(st.tmp)
	if err != nil {
		return fmt.Errorf("s3: reopen staged blob: %w", err)
	}
	defer func() { _ = f.Close() }()
	// The size is known, so large files go up as a multipart upload, which
	// the object store only turns into an object once every part arrived.
	if _, err := st.store.client.PutObject(ctx, st.store.bucket, key, f, st.info.Size,
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		return fmt.Errorf("s3: upload %s: %w", st.info.SHA256, err)
	}
	_ = f.Close()
	st.Discard()
	return nil
}

func (st *s3Staged) Discard() {
	if st.done {
		return
	}
	st.done = true
	_ = os.Remove(st.tmp)
}
