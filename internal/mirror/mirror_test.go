// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1 by definition.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
)

// The server package tests mirroring between two registries. The tests
// here use a hand-written hub, to send what a registry never would.

const hubSHA = "1111111111111111111111111111111111111111"

// hub is a minimal upstream holding one revision of acme/tiny. Its files
// are listed the way a hub lists files kept in git, by git blob id, unless
// they are named in lfs, which lists them by SHA-256 as well.
type hub struct {
	*httptest.Server
	files map[string]string
	lfs   map[string]bool
	// info is extra members for the model description.
	info map[string]any
	// wrap, when set, sees every request first; returning true means it
	// answered.
	wrap func(w http.ResponseWriter, r *http.Request) bool
}

func gitBlobID(content string) string {
	h := sha1.New() //nolint:gosec // see import note.
	fmt.Fprintf(h, "blob %d\x00%s", len(content), content)
	return hex.EncodeToString(h.Sum(nil))
}

func newHub(t *testing.T, files map[string]string) *hub {
	t.Helper()
	h := &hub{files: files}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.wrap != nil && h.wrap(w, r) {
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/acme/tiny/revision/"):
			doc := map[string]any{"id": "acme/tiny", "sha": hubSHA}
			for k, v := range h.info {
				doc[k] = v
			}
			_ = json.NewEncoder(w).Encode(doc)
		case r.URL.Path == "/api/models/acme/tiny/tree/"+hubSHA:
			_ = json.NewEncoder(w).Encode(h.listing())
		case strings.HasPrefix(r.URL.Path, "/acme/tiny/resolve/"+hubSHA+"/"):
			content, ok := h.files[strings.TrimPrefix(r.URL.Path, "/acme/tiny/resolve/"+hubSHA+"/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(content))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hub) listing() []map[string]any {
	paths := make([]string, 0, len(h.files))
	for p := range h.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]map[string]any, len(paths))
	for i, p := range paths {
		out[i] = map[string]any{"type": "file", "path": p, "size": len(h.files[p]), "oid": gitBlobID(h.files[p])}
		if h.lfs[p] {
			out[i]["lfs"] = map[string]any{"oid": sha256Hex(h.files[p]), "size": len(h.files[p])}
		}
	}
	return out
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func openRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// The upstream's model description is kept as provenance. One that is
// unreasonably large is dropped rather than stored with every revision.
func TestOversizedModelInfoIsNotStored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name string
		pad  int
		kept bool
	}{
		{"an ordinary description is kept", 1000, true},
		{"one over the limit is dropped", maxStoredInfo, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHub(t, map[string]string{"config.json": "{}"})
			h.info = map[string]any{"pipeline_tag": "text-generation", "padding": strings.Repeat("x", tc.pad)}
			up := &Upstream{Endpoint: h.URL}

			check := func(how string, reg *registry.Registry) {
				c, err := reg.Resolve(ctx, "acme/tiny", hubSHA)
				if err != nil {
					t.Fatalf("%s: %v", how, err)
				}
				if got := strings.Contains(c.UpstreamInfo, "text-generation"); got != tc.kept {
					t.Fatalf("%s: stored %d bytes of model description, kept = %v, want %v", how, len(c.UpstreamInfo), got, tc.kept)
				}
				if !tc.kept && c.UpstreamInfo != "" {
					t.Fatalf("%s: stored %d bytes; want none rather than part", how, len(c.UpstreamInfo))
				}
			}

			mirrored := openRegistry(t)
			plan, err := Prepare(ctx, up, "acme/tiny", "main")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plan.Execute(ctx, mirrored, 2, nil); err != nil {
				t.Fatal(err)
			}
			check("mirror", mirrored)

			pulled := openRegistry(t)
			if _, err := plan.RecordLazy(ctx, pulled); err != nil {
				t.Fatal(err)
			}
			check("pull-through", pulled)
		})
	}
}

// A mirror of a revision the registry already holds, recorded by
// pull-through with its files still to fetch, completes that revision. If
// it cannot be completed the mirror fails with the reason; it does not go
// on to record the revision afresh, which is only for one the registry
// does not hold.
func TestMirrorCompletesARecordedRevisionOrFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("completes it and moves the branch", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, map[string]string{"config.json": "{}", "weights.bin": "weights"})
		h.lfs = map[string]bool{"weights.bin": true}
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", hubSHA)
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := plan.RecordLazy(ctx, reg)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := reg.PendingCount(ctx, recorded); n != 2 {
			t.Fatalf("%d files pending before the mirror, want 2", n)
		}
		named, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := named.Execute(ctx, reg, 2, nil); err != nil {
			t.Fatal(err)
		}
		head, err := reg.Resolve(ctx, "acme/tiny", "main")
		if err != nil || head.ID != recorded.ID {
			t.Fatalf("main = %+v, %v; want the revision that was already recorded", head, err)
		}
		if n, _ := reg.PendingCount(ctx, head); n != 0 {
			t.Fatalf("%d files still pending after the mirror", n)
		}
	})

	t.Run("a new repository is recorded whole", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, map[string]string{"config.json": "{}"})
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Execute(ctx, reg, 1, nil); err != nil {
			t.Fatal(err)
		}
		if c, err := reg.Resolve(ctx, "acme/tiny", "main"); err != nil || c.SHA != hubSHA || c.Source != registry.SourceMirror {
			t.Fatalf("main = %+v, %v", c, err)
		}
	})

	t.Run("fails when the recorded revision cannot be completed", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, map[string]string{"config.json": "{}", "weights.bin": "weights"})
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := plan.RecordLazy(ctx, reg)
		if err != nil {
			t.Fatal(err)
		}
		// The upstream now lists fewer files under the same revision id, so
		// what the mirror fetches cannot account for the whole revision.
		delete(h.files, "weights.bin")
		shorter, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		_, err = shorter.Execute(ctx, reg, 1, nil)
		if !errors.Is(err, registry.ErrConflict) || !strings.Contains(err.Error(), "cannot be completed") {
			t.Fatalf("mirror = %v, want the registry's reason for not completing the revision", err)
		}
		if n, _ := reg.PendingCount(ctx, recorded); n != 2 {
			t.Fatalf("%d files pending after the failed mirror, want both: part of it was kept", n)
		}
	})
}
