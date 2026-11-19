// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// verifyLine is one line of the verify route's answer.
type verifyLine struct {
	Event  string   `json:"event"`
	SHA256 string   `json:"sha256"`
	Size   int64    `json:"size"`
	Status string   `json:"status"`
	Detail string   `json:"detail"`
	Repos  []string `json:"repos"`
	Blobs  int      `json:"blobs"`
	Bytes  int64    `json:"bytes"`
	Failed int      `json:"failed"`
	Error  string   `json:"error"`
}

// verify runs the check and returns the first line, the per-blob lines by
// digest, and the last line.
func verify(t *testing.T, in *regtest.Instance) (start verifyLine, blobs map[string]verifyLine, last verifyLine) {
	t.Helper()
	resp := do(t, http.MethodPost, in.URL+"/api/v1/verify", in.Admin, nil, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	blobs = map[string]verifyLine{}
	sc := bufio.NewScanner(resp.Body)
	first := true
	for sc.Scan() {
		var line verifyLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("unreadable line %q: %v", sc.Text(), err)
		}
		if first {
			start, first = line, false
			continue
		}
		if line.Event == "blob" {
			blobs[line.SHA256] = line
		}
		last = line
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if start.Event != "start" {
		t.Fatalf("first line is %+v, want the start event", start)
	}
	return start, blobs, last
}

// Each storage backend, with the means to change what it holds without
// going through the registry.
var damageableStores = []struct {
	name string
	open func(t *testing.T) (store blob.Store, overwrite func(digest string, data []byte), remove func(digest string))
}{
	{"filesystem", func(t *testing.T) (blob.Store, func(string, []byte), func(string)) {
		root := t.TempDir()
		fs, err := blob.NewFS(root)
		if err != nil {
			t.Fatal(err)
		}
		path := func(d string) string { return filepath.Join(root, "blobs", "sha256", d[:2], d) }
		return fs, func(d string, data []byte) {
				if err := os.WriteFile(path(d), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}, func(d string) {
				if err := os.Remove(path(d)); err != nil {
					t.Fatal(err)
				}
			}
	}},
	{"s3", func(t *testing.T) (blob.Store, func(string, []byte), func(string)) {
		fake := blobtest.NewFakeS3(t)
		key := func(d string) string { return "registry/sha256/" + d[:2] + "/" + d }
		return fake.Store(t), func(d string, data []byte) { fake.Overwrite(t, key(d), data) }, func(d string) { fake.Remove(t, key(d)) }
	}},
}

func TestVerifyFindsMissingAndDamagedFiles(t *testing.T) {
	t.Parallel()
	weights, config, vocab := modelFiles["model.safetensors"], modelFiles["config.json"], modelFiles["tokenizer/vocab.txt"]
	const extra = "only in the second repository"

	for _, backend := range damageableStores {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store, overwrite, remove := backend.open(t)
			in := regtest.StartWith(t, server.Config{AnonymousRead: true}, store)
			push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
			// A second repository that shares the weights.
			push(t, in, "acme/other", map[string]string{"model.safetensors": weights, "extra.txt": extra}, client.PushOptions{})
			total := int64(len(weights) + len(config) + len(vocab) + len(extra))

			start, blobs, done := verify(t, in)
			if start.Blobs != 4 || start.Bytes != total {
				t.Fatalf("start = %+v, want 4 files of %d bytes", start, total)
			}
			if done.Event != "done" || done.Blobs != 4 || done.Bytes != total || done.Failed != 0 || len(blobs) != 4 {
				t.Fatalf("intact store: done = %+v with %d blob lines", done, len(blobs))
			}
			for d, b := range blobs {
				if b.Status != "ok" || b.Detail != "" || len(b.Repos) != 0 {
					t.Errorf("intact store: %s reported as %+v", d, b)
				}
			}

			// Three kinds of damage, done behind the registry's back.
			flipped := []byte(weights)
			flipped[len(flipped)/2] ^= 0x01
			overwrite(kit.SHA256Hex(weights), flipped)     // same size, one byte different
			overwrite(kit.SHA256Hex(vocab), []byte("a\n")) // cut short
			remove(kit.SHA256Hex(config))                  // gone

			_, blobs, done = verify(t, in)
			if done.Event != "done" || done.Blobs != 4 || done.Failed != 3 {
				t.Fatalf("damaged store: done = %+v", done)
			}
			want := map[string]struct {
				status, say string
				repos       []string
			}{
				kit.SHA256Hex(weights): {"damaged", "sha256", []string{"acme/other", "acme/tiny"}},
				kit.SHA256Hex(vocab):   {"damaged", "bytes", []string{"acme/tiny"}},
				kit.SHA256Hex(config):  {"missing", "nothing under this name", []string{"acme/tiny"}},
				kit.SHA256Hex(extra):   {"ok", "", nil},
			}
			for d, w := range want {
				got := blobs[d]
				sort.Strings(got.Repos)
				if got.Status != w.status || !strings.Contains(got.Detail, w.say) || strings.Join(got.Repos, ",") != strings.Join(w.repos, ",") {
					t.Errorf("%s: got %+v, want status %q mentioning %q, used by %v", d[:12], got, w.status, w.say, w.repos)
				}
			}

			// Checking changes nothing, and what is intact is still served.
			if resp := do(t, http.MethodGet, in.URL+"/acme/other/resolve/main/extra.txt", "", nil, nil); body(t, resp) != extra {
				t.Fatal("an intact file is no longer served")
			}
			// A download of the missing file names the command that finds them all.
			resp := do(t, http.MethodGet, in.URL+"/acme/tiny/resolve/main/config.json", "", nil, nil)
			if msg := body(t, resp); resp.StatusCode != http.StatusInternalServerError || !strings.Contains(msg, "modelctl verify") {
				t.Fatalf("download of a missing file: status %d: %s", resp.StatusCode, msg)
			}

			// The repair docs/operations.md describes: remove what is damaged
			// from storage, then upload the right bytes again under the
			// same digest.
			remove(kit.SHA256Hex(weights))
			remove(kit.SHA256Hex(vocab))
			for _, content := range []string{weights, vocab, config} {
				resp := do(t, http.MethodPut, in.URL+"/api/v1/blobs/"+kit.SHA256Hex(content), in.Write, strings.NewReader(content), nil)
				if resp.StatusCode != http.StatusCreated {
					t.Fatalf("re-upload: status %d (%s)", resp.StatusCode, body(t, resp))
				}
			}
			if _, _, done := verify(t, in); done.Event != "done" || done.Blobs != 4 || done.Failed != 0 {
				t.Fatalf("after the repair: done = %+v", done)
			}
			if resp := do(t, http.MethodGet, in.URL+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil); body(t, resp) != weights {
				t.Fatal("the repaired file is not served as it was")
			}
		})
	}
}

// The other repair docs/operations.md offers, for a mirrored repository:
// delete it, which removes the damaged copy, and mirror it again.
func TestDamagedMirrorIsRepairedByDeletingAndMirroringAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, hubURL, _ := upstreamHub(t, server.Config{AnonymousRead: true}, nil)
	store, overwrite, _ := damageableStores[0].open(t) // the filesystem
	local := regtest.StartWith(t, server.Config{AnonymousRead: true, Upstream: hubURL}, store)
	cl := client.New(local.URL, local.Admin)
	if _, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil); err != nil {
		t.Fatal(err)
	}
	weights := modelFiles["model.safetensors"]
	overwrite(kit.SHA256Hex(weights), []byte(strings.ToUpper(weights)))
	if _, _, done := verify(t, local); done.Failed != 1 {
		t.Fatalf("after damage: done = %+v", done)
	}

	// Mirroring again does not repair it: the registry has a record of
	// the file and does not fetch it.
	if _, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, done := verify(t, local); done.Failed != 1 {
		t.Fatalf("after mirroring again: done = %+v; the docs say this does not repair", done)
	}

	if resp := do(t, http.MethodDelete, local.URL+"/api/v1/repos/acme/tiny", local.Admin, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if _, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, done := verify(t, local); done.Blobs != len(modelFiles) || done.Failed != 0 {
		t.Fatalf("after delete and mirror: done = %+v", done)
	}
	if resp := do(t, http.MethodGet, local.URL+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil); body(t, resp) != weights {
		t.Fatal("the repaired file is not served as it was upstream")
	}
}

func TestVerifyOfAnEmptyRegistry(t *testing.T) {
	t.Parallel()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	start, blobs, done := verify(t, in)
	if start.Blobs != 0 || len(blobs) != 0 || done.Event != "done" || done.Blobs != 0 || done.Failed != 0 {
		t.Fatalf("start %+v, %d blob lines, done %+v", start, len(blobs), done)
	}
}
