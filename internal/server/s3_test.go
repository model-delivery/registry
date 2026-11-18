// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/server"
)

// The registry behaves the same whether files are on disk or in a bucket.
// These tests repeat the central flows with the S3 backend.

func TestS3BackedRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := blobtest.NewFakeS3(t)
	in := startWith(t, server.Config{AnonymousRead: true}, fake.Store(t))
	pushed := push(t, in, "acme/tiny", modelFiles, client.PushOptions{})

	// One object per distinct file, named by digest.
	keys := fake.Keys(t)
	if len(keys) != len(modelFiles) {
		t.Fatalf("bucket holds %d objects, want %d: %v", len(keys), len(modelFiles), keys)
	}
	wantKey := "registry/sha256/" + sha(modelFiles["config.json"])[:2] + "/" + sha(modelFiles["config.json"])
	if !contains(keys, wantKey) {
		t.Fatalf("bucket keys %v do not include %s", keys, wantKey)
	}

	t.Run("download streams through the registry with the usual headers", func(t *testing.T) {
		want := modelFiles["model.safetensors"]
		resp := do(t, http.MethodHead, in.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
		if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(want)) ||
			resp.Header.Get("ETag") != `"`+sha(want)+`"` || resp.Header.Get("X-Repo-Commit") != pushed.SHA {
			t.Fatalf("HEAD: status %d length %d headers %v", resp.StatusCode, resp.ContentLength, resp.Header)
		}
		resp = do(t, http.MethodGet, in.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
		if body(t, resp) != want {
			t.Fatal("GET returned different bytes")
		}
	})

	t.Run("range requests read only part of the object", func(t *testing.T) {
		want := modelFiles["model.safetensors"]
		resp := do(t, http.MethodGet, in.url+"/acme/tiny/resolve/main/model.safetensors", "", nil, map[string]string{"Range": "bytes=20000-20099"})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := body(t, resp); got != want[20000:20100] {
			t.Fatalf("range returned %d bytes", len(got))
		}
	})

	t.Run("pull verifies what came out of the bucket", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := client.New(in.url, "").Pull(ctx, "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		for p, want := range modelFiles {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil || string(got) != want {
				t.Errorf("%s differs (%v)", p, err)
			}
		}
	})

	t.Run("re-pushing uploads nothing", func(t *testing.T) {
		uploaded := 0
		_, err := client.New(in.url, in.write).Push(ctx, writeTree(t, modelFiles), "acme/copy", client.PushOptions{
			Branch: "main", Progress: func(_ string, _ int64, up bool) {
				if up {
					uploaded++
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if uploaded != 0 || len(fake.Keys(t)) != len(modelFiles) {
			t.Fatalf("uploaded %d files, bucket has %d objects", uploaded, len(fake.Keys(t)))
		}
	})

	t.Run("deleting repositories empties the bucket", func(t *testing.T) {
		for _, repo := range []string{"acme/tiny", "acme/copy"} {
			resp := do(t, http.MethodDelete, in.url+"/api/v1/repos/"+repo, in.admin, nil, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("delete %s: status %d", repo, resp.StatusCode)
			}
			// The first delete must keep everything: the copy still uses it.
			if repo == "acme/tiny" && len(fake.Keys(t)) != len(modelFiles) {
				t.Fatalf("deleting one repository removed objects another still uses: %v", fake.Keys(t))
			}
		}
		if keys := fake.Keys(t); len(keys) != 0 {
			t.Fatalf("bucket still holds %v", keys)
		}
	})
}

func TestS3MirrorVerifiesBeforeAnythingReachesTheBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// An upstream that lists correct digests but sends tampered bytes.
	_, hubURL, _ := upstreamHub(t, server.Config{AnonymousRead: true}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/model.safetensors") {
				tampered := strings.Repeat("tampered", len(modelFiles["model.safetensors"])/8)
				w.Header().Set("Content-Length", strconv.Itoa(len(tampered)))
				_, _ = w.Write([]byte(tampered))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	fake := blobtest.NewFakeS3(t)
	local := startWith(t, server.Config{AnonymousRead: true, Upstream: hubURL}, fake.Store(t))

	if _, err := client.New(local.url, local.write).Mirror(ctx, "acme/tiny", "main", "", nil); err == nil {
		t.Fatal("mirror of tampered content succeeded")
	}
	for _, key := range fake.Keys(t) {
		if strings.HasSuffix(key, sha(modelFiles["model.safetensors"])) {
			t.Fatalf("an object was created under the digest of a file that failed verification: %s", key)
		}
	}
	tampered := strings.Repeat("tampered", len(modelFiles["model.safetensors"])/8)
	for _, key := range fake.Keys(t) {
		if strings.HasSuffix(key, sha(tampered)) {
			t.Fatalf("tampered bytes were stored: %s", key)
		}
	}
}

func TestS3RedirectedDownloads(t *testing.T) {
	t.Parallel()
	fake := blobtest.NewFakeS3(t)
	in := startWith(t, server.Config{AnonymousRead: false, RedirectDownloads: true}, fake.Store(t))
	pushed := push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	want := modelFiles["model.safetensors"]
	fileURL := in.url + "/acme/tiny/resolve/main/model.safetensors"

	t.Run("the registry describes the file and points at the bucket", func(t *testing.T) {
		resp := do(t, http.MethodHead, fileURL, in.read, nil, nil)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status %d, want a redirect", resp.StatusCode)
		}
		h := resp.Header
		if h.Get("X-Repo-Commit") != pushed.SHA || h.Get("X-Linked-Etag") != `"`+sha(want)+`"` ||
			h.Get("X-Linked-Size") != strconv.Itoa(len(want)) {
			t.Fatalf("headers = %v", h)
		}
		loc := h.Get("Location")
		if !strings.HasPrefix(loc, fake.Config.Endpoint) || !strings.Contains(loc, "X-Amz-Signature=") {
			t.Fatalf("Location is not a signed bucket URL: %s", loc)
		}

		// The signed URL serves the bytes with no registry token at all.
		direct := do(t, http.MethodGet, loc, "", nil, nil)
		if direct.StatusCode != http.StatusOK || body(t, direct) != want {
			t.Fatalf("signed URL: status %d", direct.StatusCode)
		}
		// And it honours ranges, which is how clients resume.
		part := do(t, http.MethodGet, loc, "", nil, map[string]string{"Range": "bytes=100-199"})
		if part.StatusCode != http.StatusPartialContent || body(t, part) != want[100:200] {
			t.Fatalf("signed URL range: status %d", part.StatusCode)
		}
	})

	t.Run("the redirect is only given to callers allowed to read", func(t *testing.T) {
		for name, token := range map[string]string{"no token": "", "unknown token": "md_unknown_token_value_000000"} {
			resp := do(t, http.MethodGet, fileURL, token, nil, nil)
			if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Location") != "" {
				t.Errorf("%s: status %d location %q", name, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
	})

	t.Run("missing files are still reported by the registry", func(t *testing.T) {
		resp := do(t, http.MethodHead, in.url+"/acme/tiny/resolve/main/absent.bin", in.read, nil, nil)
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "EntryNotFound" {
			t.Fatalf("status %d code %q", resp.StatusCode, resp.Header.Get("X-Error-Code"))
		}
	})

	t.Run("the Go client follows the redirect and verifies the result", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := client.New(in.url, in.read).Pull(context.Background(), "acme/tiny", "main", dir, nil); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "model.safetensors"))
		if string(got) != want {
			t.Fatal("pulled file differs")
		}
	})
}

func TestRedirectSettingIsIgnoredOnLocalStorage(t *testing.T) {
	t.Parallel()
	// Local files cannot be signed, so the registry keeps serving them.
	in := start(t, server.Config{AnonymousRead: true, RedirectDownloads: true})
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	resp := do(t, http.MethodGet, in.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["config.json"] {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
