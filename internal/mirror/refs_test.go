// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
)

// Revision ids of the refHub tests.
const (
	revA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	revB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	revC = "cccccccccccccccccccccccccccccccccccccccc"
)

// refHub is an upstream holding one repository with several revisions,
// branches and tags, all of which a test may change between mirrors. Files
// ending in .bin are listed as LFS files.
type refHub struct {
	*httptest.Server
	key string // the repository's key: acme/tiny or datasets/acme/tiny

	mu       sync.Mutex
	revs     map[string]map[string]string // revision id -> path -> content
	branches map[string]string            // name -> revision id
	tags     map[string]string
	// brokenTree holds revisions whose file listing fails; corrupt holds
	// paths whose content is sent altered.
	brokenTree map[string]bool
	corrupt    map[string]bool

	files      atomic.Int64 // file downloads
	inFlight   atomic.Int64 // file downloads in progress now
	mostAtOnce atomic.Int64 // the most file downloads there have been at once
}

func newRefHub(t *testing.T, typ registry.RepoType) *refHub {
	t.Helper()
	h := &refHub{
		key:  registry.RepoKey(typ, "acme/tiny"),
		revs: map[string]map[string]string{}, branches: map[string]string{}, tags: map[string]string{},
		brokenTree: map[string]bool{}, corrupt: map[string]bool{},
	}
	api := "/api/models/acme/tiny"
	if typ == registry.TypeDataset {
		api = "/api/datasets/acme/tiny"
	}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		path := r.URL.Path
		switch {
		case path == api+"/refs":
			type ref struct {
				Name         string `json:"name"`
				TargetCommit string `json:"targetCommit"`
			}
			list := func(m map[string]string) []ref {
				out := []ref{}
				for name, sha := range m {
					out = append(out, ref{name, sha})
				}
				sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
				return out
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"branches": list(h.branches), "tags": list(h.tags), "converts": []ref{}})
		case strings.HasPrefix(path, api+"/revision/"):
			rev, _ := url.PathUnescape(strings.TrimPrefix(path, api+"/revision/"))
			sha := rev
			if s, ok := h.branches[rev]; ok {
				sha = s
			} else if s, ok := h.tags[rev]; ok {
				sha = s
			}
			if _, ok := h.revs[sha]; !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "acme/tiny", "sha": sha})
		case strings.HasPrefix(path, api+"/tree/"):
			sha := strings.TrimPrefix(path, api+"/tree/")
			files, ok := h.revs[sha]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if h.brokenTree[sha] {
				http.Error(w, "broken", http.StatusInternalServerError)
				return
			}
			var out []map[string]any
			for p, content := range files {
				e := map[string]any{"type": "file", "path": p, "size": len(content), "oid": gitBlobID(content)}
				if strings.HasSuffix(p, ".bin") {
					e["lfs"] = map[string]any{"oid": kit.SHA256Hex(content), "size": len(content)}
				}
				out = append(out, e)
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(path, "/"+h.key+"/resolve/"):
			rest := strings.TrimPrefix(path, "/"+h.key+"/resolve/")
			sha, p, _ := strings.Cut(rest, "/")
			content, ok := h.revs[sha][p]
			if !ok {
				http.NotFound(w, r)
				return
			}
			h.files.Add(1)
			now := h.inFlight.Add(1)
			for {
				most := h.mostAtOnce.Load()
				if now <= most || h.mostAtOnce.CompareAndSwap(most, now) {
					break
				}
			}
			corrupt := h.corrupt[p]
			h.mu.Unlock()
			if corrupt {
				content = strings.ToUpper(content)
			}
			_, _ = w.Write([]byte(content))
			h.inFlight.Add(-1)
			h.mu.Lock()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

// set changes the hub under the lock: f may change anything in it.
func (h *refHub) set(f func(h *refHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// outcomes is each ref's outcome, by name.
func outcomes(res RefsResult) map[string]RefOutcome {
	out := map[string]RefOutcome{}
	for _, r := range res.Refs {
		out[r.Name] = r.Outcome
	}
	return out
}

func checkRef(t *testing.T, reg *registry.Registry, key, name string, kind registry.RefKind, sha string) {
	t.Helper()
	ref, err := reg.Ref(context.Background(), key, name)
	if err != nil || ref.Kind != kind || ref.SHA != sha {
		t.Fatalf("%s@%s = %+v, %v; want a %s at %s", key, name, ref, err, kind, sha)
	}
}

// Every branch and every tag, of a model and of a dataset: each recorded
// as the kind of ref it is upstream, files shared between revisions stored
// and fetched once, and a second run fetching nothing.
func TestMirrorEveryRef(t *testing.T) {
	t.Parallel()
	for _, typ := range []registry.RepoType{registry.TypeModel, registry.TypeDataset} {
		t.Run(string(typ), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newRefHub(t, typ)
			h.set(func(h *refHub) {
				h.revs[revA] = map[string]string{"config.json": "{}", "weights.bin": "weights one"}
				h.revs[revB] = map[string]string{"config.json": "{}", "weights.bin": "weights two"}
				h.branches["main"], h.branches["dev"] = revB, revA
				h.tags["v1"], h.tags["v2"] = revA, revB
			})
			reg := openRegistry(t)
			up := &Upstream{Endpoint: h.URL}
			var lines []Event
			var mu sync.Mutex
			res, err := MirrorRefs(ctx, up, reg, h.key, registry.FollowSpec{AllRefs: true}, Options{Progress: func(e Event) {
				mu.Lock()
				lines = append(lines, e)
				mu.Unlock()
			}})
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]RefOutcome{"main": RefMirrored, "dev": RefMirrored, "v1": RefMirrored, "v2": RefMirrored}
			if got := outcomes(res); len(got) != len(want) || got["main"] != RefMirrored || got["dev"] != RefMirrored || got["v1"] != RefMirrored || got["v2"] != RefMirrored {
				t.Fatalf("outcomes %v, want %v", got, want)
			}
			checkRef(t, reg, h.key, "main", registry.KindBranch, revB)
			checkRef(t, reg, h.key, "dev", registry.KindBranch, revA)
			checkRef(t, reg, h.key, "v1", registry.KindTag, revA)
			checkRef(t, reg, h.key, "v2", registry.KindTag, revB)
			// Three distinct contents, each fetched once: config.json is in
			// both revisions, and each revision is named by two refs.
			if n := h.files.Load(); n != 3 {
				t.Fatalf("%d file downloads, want 3", n)
			}
			for _, e := range lines {
				if e.Event == EventDone {
					t.Fatalf("a done line among the progress: %+v", e)
				}
			}
			refLines := 0
			for _, e := range lines {
				if e.Event == EventRef {
					refLines++
				}
			}
			if refLines != 4 {
				t.Fatalf("%d ref lines, want 4", refLines)
			}

			again, err := MirrorRefs(ctx, up, reg, h.key, registry.FollowSpec{AllRefs: true}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			for name, o := range outcomes(again) {
				if o != RefUnchanged {
					t.Errorf("second run: %s %s", name, o)
				}
			}
			if again.Changed() || h.files.Load() != 3 {
				t.Fatalf("second run changed %v and made %d file downloads in all", again.Changed(), h.files.Load())
			}
		})
	}
}

// A followed branch that moves upstream: the new revision is mirrored with
// only its changed files fetched, the branch moves, and the old revision
// stays resolvable by its id.
func TestMirrorABranchThatMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefHub(t, registry.TypeModel)
	h.set(func(h *refHub) {
		h.revs[revA] = map[string]string{"config.json": "{}", "weights.bin": "weights one", "tokenizer.json": "tok"}
		h.branches["main"] = revA
	})
	reg := openRegistry(t)
	up := &Upstream{Endpoint: h.URL}
	branch := registry.FollowSpec{Branch: "main"}
	if _, err := MirrorRefs(ctx, up, reg, h.key, branch, Options{}); err != nil {
		t.Fatal(err)
	}
	before := h.files.Load()

	h.set(func(h *refHub) {
		h.revs[revB] = map[string]string{"config.json": "{}", "weights.bin": "weights two", "tokenizer.json": "tok"}
		h.branches["main"] = revB
	})
	res, err := MirrorRefs(ctx, up, reg, h.key, branch, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Refs) != 1 || res.Refs[0].Outcome != RefMirrored || res.Refs[0].Previous != revA || res.Refs[0].SHA != revB {
		t.Fatalf("result %+v", res.Refs)
	}
	if got := h.files.Load() - before; got != 1 {
		t.Fatalf("%d files downloaded for the new revision, want only the changed one", got)
	}
	checkRef(t, reg, h.key, "main", registry.KindBranch, revB)
	old, err := reg.Resolve(ctx, h.key, revA)
	if err != nil {
		t.Fatalf("the earlier revision: %v", err)
	}
	if f, err := reg.File(ctx, old, "weights.bin"); err != nil || f.SHA256 != kit.SHA256Hex("weights one") {
		t.Fatalf("the earlier revision's weights: %+v, %v", f, err)
	}
}

// A tag that names another revision upstream stays where it is, and says
// so; a ref deleted upstream stays too.
func TestMirrorRefsNeverMovesATagOrDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefHub(t, registry.TypeModel)
	h.set(func(h *refHub) {
		h.revs[revA] = map[string]string{"config.json": "{}"}
		h.revs[revB] = map[string]string{"config.json": "{\"v\":2}"}
		h.branches["main"], h.branches["dev"] = revA, revA
		h.tags["v1"] = revA
	})
	reg := openRegistry(t)
	up := &Upstream{Endpoint: h.URL}
	all := registry.FollowSpec{AllRefs: true}
	if _, err := MirrorRefs(ctx, up, reg, h.key, all, Options{}); err != nil {
		t.Fatal(err)
	}

	h.set(func(h *refHub) {
		h.tags["v1"] = revB
		delete(h.branches, "dev")
	})
	res, err := MirrorRefs(ctx, up, reg, h.key, all, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["v1"] != RefTagMoved || got["dev"] != RefDeletedUpstream || got["main"] != RefUnchanged || len(res.Failed()) != 0 {
		t.Fatalf("outcomes %v", got)
	}
	for _, r := range res.Refs {
		if r.Name == "v1" && (!strings.Contains(r.Detail, revA) || !strings.Contains(r.Detail, revB) || r.Previous != revA) {
			t.Fatalf("the moved tag is reported as %+v", r)
		}
	}
	checkRef(t, reg, h.key, "v1", registry.KindTag, revA)
	checkRef(t, reg, h.key, "dev", registry.KindBranch, revA)

	// Following the deleted branch alone says the same.
	res, err = MirrorRefs(ctx, up, reg, h.key, registry.FollowSpec{Branch: "dev"}, Options{})
	if err != nil || len(res.Refs) != 1 || res.Refs[0].Outcome != RefDeletedUpstream || res.Refs[0].Previous != revA {
		t.Fatalf("the deleted branch alone: %+v, %v", res.Refs, err)
	}

	// A single mirror of the moved tag is refused before it fetches.
	plan, err := Prepare(ctx, up, h.key, "v1")
	if err != nil {
		t.Fatal(err)
	}
	before := h.files.Load()
	var moved *TagMovedError
	if _, err := plan.Execute(ctx, reg, 1, nil); !errors.As(err, &moved) || !errors.Is(err, registry.ErrConflict) || moved.Local != revA {
		t.Fatalf("mirroring the moved tag: %v", err)
	}
	if h.files.Load() != before {
		t.Fatal("the refused mirror fetched files")
	}
	checkRef(t, reg, h.key, "v1", registry.KindTag, revA)
}

// One ref that fails costs only itself: the others are recorded, and the
// failed one records nothing, verification failures included.
func TestMirrorRefsOneFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefHub(t, registry.TypeDataset)
	h.set(func(h *refHub) {
		h.revs[revA] = map[string]string{"train.csv": "a,b"}
		h.revs[revB] = map[string]string{"test.csv": "c,d"}
		h.revs[revC] = map[string]string{"bad.csv": "e,f"}
		h.branches["main"], h.branches["broken"], h.branches["corrupt"] = revA, revB, revC
		h.brokenTree[revB] = true
		h.corrupt["bad.csv"] = true
	})
	reg := openRegistry(t)
	res, err := MirrorRefs(ctx, &Upstream{Endpoint: h.URL}, reg, h.key, registry.FollowSpec{AllRefs: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(res)
	if got["main"] != RefMirrored || got["broken"] != RefFailed || got["corrupt"] != RefFailed || len(res.Failed()) != 2 {
		t.Fatalf("outcomes %v", got)
	}
	for _, r := range res.Failed() {
		if !FromUpstream(r.Err) || r.Detail == "" {
			t.Errorf("%s failed with %v (%q), want the upstream's failure", r.Name, r.Err, r.Detail)
		}
	}
	checkRef(t, reg, h.key, "main", registry.KindBranch, revA)
	for _, sha := range []string{revB, revC} {
		if _, err := reg.Resolve(ctx, h.key, sha); !errors.Is(err, registry.ErrRevisionNotFound) {
			t.Fatalf("revision %s of a failed ref: %v", sha, err)
		}
	}
	for _, name := range []string{"broken", "corrupt"} {
		if _, err := reg.Ref(ctx, h.key, name); !errors.Is(err, registry.ErrRevisionNotFound) {
			t.Fatalf("ref %s of a failure: %v", name, err)
		}
	}
}

// Mirroring a tag by name records a tag, by a mirror and by pull-through
// alike; a branch by name is still a branch.
func TestATagStaysATag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefHub(t, registry.TypeModel)
	h.set(func(h *refHub) {
		h.revs[revA] = map[string]string{"config.json": "{}"}
		h.branches["main"] = revA
		h.tags["v1"] = revA
	})
	up := &Upstream{Endpoint: h.URL}

	mirrored := openRegistry(t)
	for _, name := range []string{"v1", "main"} {
		plan, err := Prepare(ctx, up, h.key, name)
		if err != nil {
			t.Fatal(err)
		}
		res, err := plan.Execute(ctx, mirrored, 1, nil)
		if err != nil || res.Ref != name {
			t.Fatalf("mirror %s: %+v, %v", name, res, err)
		}
	}
	checkRef(t, mirrored, h.key, "v1", registry.KindTag, revA)
	checkRef(t, mirrored, h.key, "main", registry.KindBranch, revA)

	pulled := openRegistry(t)
	plan, err := Prepare(ctx, up, h.key, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.RecordLazy(ctx, pulled); err != nil {
		t.Fatal(err)
	}
	checkRef(t, pulled, h.key, "v1", registry.KindTag, revA)
}

// A mirror given file slots never has more files in flight than it holds
// slots, however many it is allowed to fetch at once.
func TestMirrorHoldsAFileSlotPerFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newRefHub(t, registry.TypeModel)
	files := map[string]string{}
	for _, p := range []string{"a.bin", "b.bin", "c.bin", "d.bin", "e.bin", "f.bin"} {
		files[p] = "content of " + p
	}
	h.set(func(h *refHub) {
		h.revs[revA] = files
		h.branches["main"] = revA
	})
	slots := make(chan struct{}, 1)
	slot := func(ctx context.Context) (func(), error) {
		select {
		case slots <- struct{}{}:
			return func() { <-slots }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	listings := 0
	listing := func(context.Context) (func(), error) { listings++; return func() {}, nil }
	res, err := MirrorRefs(ctx, &Upstream{Endpoint: h.URL}, openRegistry(t), h.key, registry.FollowSpec{Branch: "main"},
		Options{Concurrency: 4, FileSlot: slot, ListingSlot: listing})
	if err != nil || res.Refs[0].Outcome != RefMirrored {
		t.Fatalf("%+v, %v", res, err)
	}
	if most := h.mostAtOnce.Load(); most != 1 {
		t.Fatalf("%d file downloads at once with one slot", most)
	}
	if listings != 2 {
		t.Fatalf("%d listing slots taken, want 2: the refs, and the revision", listings)
	}
}
