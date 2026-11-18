// SPDX-License-Identifier: Apache-2.0

package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/blob/blobtest"
)

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Known git blob ids, from `git hash-object`.
const (
	gitOIDHello = "ce013625030ba8dba906f756967f9e9ca394464a" // "hello\n"
	gitOIDEmpty = "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" // ""
)

// backend is one storage implementation under test.
type backend struct {
	name string
	// open returns a store and the directory uploads are staged in.
	open func(t *testing.T) (store blob.Store, stagingDir string)
}

// Every test below runs against every backend: the contract is the same.
var backends = []backend{
	{"filesystem", func(t *testing.T) (blob.Store, string) {
		root := t.TempDir()
		s, err := blob.NewFS(root)
		if err != nil {
			t.Fatal(err)
		}
		return s, filepath.Join(root, "tmp")
	}},
	{"s3", func(t *testing.T) (blob.Store, string) {
		fake := blobtest.NewFakeS3(t)
		return fake.Store(t), fake.Config.StagingDir
	}},
}

// realS3 adds a real S3-compatible service to the backends under test when
// REGISTRY_TEST_S3_ENDPOINT is set (with REGISTRY_TEST_S3_BUCKET,
// REGISTRY_TEST_S3_ACCESS_KEY and REGISTRY_TEST_S3_SECRET_KEY). The bucket
// is created if it is missing. `task test:s3` starts a server and sets these.
func realS3() []backend {
	endpoint := os.Getenv("REGISTRY_TEST_S3_ENDPOINT")
	if endpoint == "" {
		return nil
	}
	return []backend{{"s3-real", func(t *testing.T) (blob.Store, string) {
		u, err := url.Parse(endpoint)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := minio.New(u.Host, &minio.Options{
			Creds:  credentials.NewStaticV4(os.Getenv("REGISTRY_TEST_S3_ACCESS_KEY"), os.Getenv("REGISTRY_TEST_S3_SECRET_KEY"), ""),
			Secure: u.Scheme == "https", Region: "us-east-1",
		})
		if err != nil {
			t.Fatal(err)
		}
		bucket := os.Getenv("REGISTRY_TEST_S3_BUCKET")
		if exists, err := admin.BucketExists(context.Background(), bucket); err != nil {
			t.Fatal(err)
		} else if !exists {
			// Another parallel test may create it first; that is fine.
			_ = admin.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{Region: "us-east-1"})
		}
		cfg := blob.S3Config{
			Endpoint:   endpoint,
			Region:     "us-east-1",
			Bucket:     os.Getenv("REGISTRY_TEST_S3_BUCKET"),
			Prefix:     "test-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000"),
			AccessKey:  os.Getenv("REGISTRY_TEST_S3_ACCESS_KEY"),
			SecretKey:  os.Getenv("REGISTRY_TEST_S3_SECRET_KEY"),
			StagingDir: t.TempDir(),
		}
		s, err := blob.NewS3(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return s, cfg.StagingDir
	}}}
}

func eachBackend(t *testing.T, fn func(t *testing.T, store blob.Store, stagingDir string)) {
	t.Helper()
	for _, b := range append(backends, realS3()...) {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			store, staging := b.open(t)
			fn(t, store, staging)
		})
	}
}

func stagedFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func readAll(t *testing.T, store blob.Store, d string) []byte {
	t.Helper()
	f, _, err := store.Open(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestStageVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hello := []byte("hello\n")
	tests := []struct {
		name    string
		data    []byte
		want    blob.Expect
		wantErr bool
	}{
		{"no expectations", hello, blob.Expect{Size: -1}, false},
		{"all correct", hello, blob.Expect{SHA256: digest(hello), GitOID: gitOIDHello, Size: 6}, false},
		{"git id checked without a known size", hello, blob.Expect{GitOID: gitOIDHello, Size: -1}, false},
		{"empty file", nil, blob.Expect{GitOID: gitOIDEmpty, Size: 0}, false},
		{"wrong sha256", hello, blob.Expect{SHA256: digest([]byte("other")), Size: -1}, true},
		{"wrong git id", hello, blob.Expect{GitOID: gitOIDEmpty, Size: -1}, true},
		{"shorter than declared", hello, blob.Expect{Size: 7}, true},
		{"longer than declared", hello, blob.Expect{Size: 5}, true},
	}
	eachBackend(t, func(t *testing.T, store blob.Store, staging string) {
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				st, err := store.Stage(ctx, bytes.NewReader(tc.data), tc.want)
				if tc.wantErr {
					if !errors.Is(err, blob.ErrMismatch) {
						t.Fatalf("got error %v, want ErrMismatch", err)
					}
					// A rejected upload leaves nothing behind, staged or stored.
					if n := stagedFiles(t, staging); n != 0 {
						t.Fatalf("rejected upload left %d staging files", n)
					}
					if _, _, err := store.Open(ctx, digest(tc.data)); !errors.Is(err, blob.ErrNotFound) {
						t.Fatalf("rejected upload is readable: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				defer st.Discard()
				info := st.Info()
				if info.SHA256 != digest(tc.data) || info.Size != int64(len(tc.data)) {
					t.Fatalf("info = %+v", info)
				}
				// Not visible until published.
				if _, _, err := store.Open(ctx, info.SHA256); !errors.Is(err, blob.ErrNotFound) {
					t.Fatalf("staged blob visible before publish: %v", err)
				}
				if err := st.Publish(ctx); err != nil {
					t.Fatal(err)
				}
				if got := readAll(t, store, info.SHA256); !bytes.Equal(got, tc.data) {
					t.Fatalf("read back %q", got)
				}
				if n := stagedFiles(t, staging); n != 0 {
					t.Fatalf("%d staging files left after publish", n)
				}
				// Clean up so the next case starts from an empty store.
				if err := store.Delete(ctx, info.SHA256); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

func TestOpenReportsSizeAndSeeks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := []byte(strings.Repeat("0123456789", 5000))
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{Size: -1})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		f, size, err := store.Open(ctx, digest(data))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if size != int64(len(data)) {
			t.Fatalf("size = %d, want %d", size, len(data))
		}
		// A range request reads from the middle of the blob.
		if _, err := f.Seek(40_005, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 10)
		if _, err := io.ReadFull(f, buf); err != nil {
			t.Fatal(err)
		}
		if string(buf) != "5678901234" {
			t.Fatalf("read %q at offset 40005", buf)
		}
	})
}

// TestLargeBlobRoundTrip sends a blob big enough that the S3 backend has to
// split it into a multipart upload, and checks it comes back bit for bit.
func TestLargeBlobRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const size = 40 << 20
	// A repeating but non-aligned pattern, so misplaced parts would show.
	pattern := []byte("model-delivery/large-blob/0123456789abcdefghijklmnopqrstuvwxyz\n")
	source := func() io.Reader {
		return io.LimitReader(repeat(pattern), size)
	}
	h := sha256.New()
	if _, err := io.Copy(h, source()); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))

	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		if strings.HasSuffix(t.Name(), "/s3") {
			// The in-memory fake does not decode signed multipart uploads.
			// Multipart is covered by the real-service run (task test:s3).
			t.Skip("the in-memory S3 fake cannot receive multipart uploads")
		}
		st, err := store.Stage(ctx, source(), blob.Expect{SHA256: want, Size: size})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Discard()
		if err := st.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		f, gotSize, err := store.Open(ctx, want)
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.New()
		_, err = io.Copy(got, f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if gotSize != size || hex.EncodeToString(got.Sum(nil)) != want {
			t.Fatalf("large blob came back different (size %d)", gotSize)
		}
		if err := store.Delete(ctx, want); err != nil {
			t.Fatal(err)
		}
	})
}

// repeat returns a reader that yields p over and over.
func repeat(p []byte) io.Reader { return &repeater{p: p} }

type repeater struct {
	p   []byte
	off int
}

func (r *repeater) Read(b []byte) (int, error) {
	n := 0
	for n < len(b) {
		c := copy(b[n:], r.p[r.off:])
		n += c
		r.off = (r.off + c) % len(r.p)
	}
	return n, nil
}

func TestConcurrentIdenticalUploadsStoreOneCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := []byte(strings.Repeat("same bytes ", 4096))
	eachBackend(t, func(t *testing.T, store blob.Store, staging string) {
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{SHA256: digest(data), Size: int64(len(data))})
				if err == nil {
					err = st.Publish(ctx)
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := stagedFiles(t, staging); n != 0 {
			t.Fatalf("%d staging files left after identical uploads", n)
		}
		if got := readAll(t, store, digest(data)); !bytes.Equal(got, data) {
			t.Fatal("stored copy differs from the upload")
		}
	})
}

func TestMalformedDigestsAreNeverLookedUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		for _, bad := range []string{"", "abc", "../../etc/passwd", strings.Repeat("Z", 64), strings.Repeat("A", 64)} {
			if _, _, err := store.Open(ctx, bad); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("Open(%q) = %v, want ErrNotFound", bad, err)
			}
			if err := store.Delete(ctx, bad); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("Delete(%q) = %v, want ErrNotFound", bad, err)
			}
		}
	})
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	eachBackend(t, func(t *testing.T, store blob.Store, _ string) {
		if err := store.Delete(ctx, digest([]byte("never stored"))); err != nil {
			t.Fatalf("deleting an absent blob: %v", err)
		}
		data := []byte("to be removed")
		st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{Size: -1})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, digest(data)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Open(ctx, digest(data)); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("deleted blob is still readable: %v", err)
		}
	})
}

func TestFilesystemStoresBlobsByDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	store, err := blob.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("model weights")
	st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	d := digest(data)
	if _, err := os.Stat(filepath.Join(root, "blobs", "sha256", d[:2], d)); err != nil {
		t.Fatalf("blob is not at its digest path: %v", err)
	}
}

func TestS3StoresObjectsByDigestUnderPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := blobtest.NewFakeS3(t)
	store := fake.Store(t)
	data := []byte("model weights")
	st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	// Staging alone sends nothing to the bucket.
	if keys := fake.Keys(t); len(keys) != 0 {
		t.Fatalf("bucket has objects before publish: %v", keys)
	}
	if err := st.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	d := digest(data)
	want := "registry/sha256/" + d[:2] + "/" + d
	if keys := fake.Keys(t); len(keys) != 1 || keys[0] != want {
		t.Fatalf("bucket keys = %v, want [%s]", keys, want)
	}
}

func TestS3PresignedURLDownloadsWithoutCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := blobtest.NewFakeS3(t).Store(t)
	data := []byte("fetched straight from the bucket")
	st, err := store.Stage(ctx, bytes.NewReader(data), blob.Expect{Size: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := store.PresignGet(ctx, digest(data), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "X-Amz-Signature=") || !strings.Contains(u, "X-Amz-Expires=60") {
		t.Fatalf("URL is not a 60-second signed URL: %s", u)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("status %d, body %q", resp.StatusCode, got)
	}
}

func TestS3ConfigurationErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	good := blobtest.NewFakeS3(t).Config
	tests := []struct {
		name    string
		change  func(*blob.S3Config)
		wantErr string
	}{
		{"no bucket", func(c *blob.S3Config) { c.Bucket = "" }, "bucket name is required"},
		{"bucket that does not exist", func(c *blob.S3Config) { c.Bucket = "absent" }, `bucket "absent" does not exist`},
		{"endpoint without a scheme", func(c *blob.S3Config) { c.Endpoint = "minio:9000" }, "endpoint must be a URL"},
		{"unreachable endpoint", func(c *blob.S3Config) { c.Endpoint = "http://127.0.0.1:1" }, "cannot reach bucket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			cfg.StagingDir = t.TempDir()
			tc.change(&cfg)
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, err := blob.NewS3(ctx, cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}
