// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// syncHub is a registry playing the upstream, behind a front that counts
// file downloads and can be told to misbehave: answer the refs listing of
// a repository with a list of the test's own (to move a tag or delete a
// branch, which a registry never does), be down for one repository, send
// altered weights, or hold file downloads.
type syncHub struct {
	inst *regtest.Instance
	url  string

	files    atomic.Int64 // file downloads
	tampered atomic.Bool  // model.safetensors is sent altered

	mu       sync.Mutex
	refs     map[string]string // a path's refs listing, as JSON, in place of the real one
	down     map[string]bool   // repository names whose requests fail with 503
	hold     chan struct{}     // when set, file downloads wait until it is closed
	fetching chan struct{}     // closed when a held download has begun
	once     sync.Once
}

func newSyncHub(t *testing.T) *syncHub {
	t.Helper()
	h := &syncHub{refs: map[string]string{}, down: map[string]bool{}, fetching: make(chan struct{})}
	h.inst = regtest.Start(t, server.Config{AnonymousRead: true})
	target, err := url.Parse(h.inst.URL)
	if err != nil {
		t.Fatal(err)
	}
	next := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		listing, hold := h.refs[r.URL.Path], h.hold
		down := false
		for name, d := range h.down {
			down = down || (d && strings.Contains(r.URL.Path, name))
		}
		h.mu.Unlock()
		switch {
		case down:
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
			return
		case listing != "":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, listing)
			return
		case strings.Contains(r.URL.Path, "/resolve/"):
			h.files.Add(1)
			if hold != nil {
				h.once.Do(func() { close(h.fetching) })
				select {
				case <-hold:
				case <-r.Context().Done():
					return
				}
			}
			if h.tampered.Load() && strings.HasSuffix(r.URL.Path, "/model.safetensors") {
				_, _ = io.WriteString(w, strings.Repeat("x", len(modelFiles["model.safetensors"])))
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	h.url = front.URL
	return h
}

// fakeRefs makes the hub answer the refs listing of key with these
// branches and tags, by name and revision id; nil maps restore the real
// listing.
func (h *syncHub) fakeRefs(key string, branches, tags map[string]string) {
	typ, name := registry.SplitRepoKey(key)
	path := "/api/models/" + name + "/refs"
	if typ == registry.TypeDataset {
		path = "/api/datasets/" + name + "/refs"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if branches == nil && tags == nil {
		delete(h.refs, path)
		return
	}
	list := func(m map[string]string) []map[string]string {
		out := []map[string]string{}
		for n, sha := range m {
			out = append(out, map[string]string{"name": n, "ref": "refs/x/" + n, "targetCommit": sha})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
		return out
	}
	raw, _ := json.Marshal(map[string]any{"branches": list(branches), "tags": list(tags), "converts": []any{}})
	h.refs[path] = string(raw)
}

func (h *syncHub) setDown(name string, down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down[name] = down
}

// apiRoot is the registry API address of the repository with key key.
func apiRoot(key string) string {
	if typ, name := registry.SplitRepoKey(key); typ == registry.TypeDataset {
		return "/api/v1/datasets/" + name
	}
	return "/api/v1/repos/" + key
}

// syncDoc is the answer to a sync of one repository.
type syncDoc struct {
	Name            string `json:"name"`
	Changed         bool   `json:"changed"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	Error           string `json:"error"`
	Refs            []struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Outcome  string `json:"outcome"`
		SHA      string `json:"sha"`
		Previous string `json:"previous"`
		Detail   string `json:"detail"`
	} `json:"refs"`
}

// outcome is the outcome the sync reported for ref.
func (d syncDoc) outcome(ref string) string {
	for _, r := range d.Refs {
		if r.Name == ref {
			return r.Outcome
		}
	}
	return ""
}

func follow(t *testing.T, in *regtest.Instance, key, spec string) *http.Response {
	t.Helper()
	return do(t, http.MethodPost, in.URL+apiRoot(key)+"/follow", in.Write, strings.NewReader(spec), nil)
}

func mustFollow(t *testing.T, in *regtest.Instance, key, spec string) {
	t.Helper()
	if resp := follow(t, in, key, spec); resp.StatusCode != http.StatusOK {
		t.Fatalf("follow %s with %s: status %d (%s)", key, spec, resp.StatusCode, body(t, resp))
	}
}

func syncNow(t *testing.T, in *regtest.Instance, key string) (int, syncDoc) {
	t.Helper()
	resp := do(t, http.MethodPost, in.URL+apiRoot(key)+"/sync", in.Write, nil, nil)
	var d syncDoc
	raw := body(t, resp)
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("sync %s: status %d, %s", key, resp.StatusCode, raw)
	}
	return resp.StatusCode, d
}

func followEntry(t *testing.T, in *regtest.Instance, key string) registry.Follow {
	t.Helper()
	f, err := in.Reg.GetFollow(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mirrorMain(t *testing.T, in *regtest.Instance, key string) {
	t.Helper()
	if _, err := client.New(in.URL, in.Write).Mirror(context.Background(), key, "main", "", nil); err != nil {
		t.Fatal(err)
	}
}

// with is modelFiles with one file changed.
func with(path, content string) map[string]string {
	out := map[string]string{}
	for p, c := range modelFiles {
		out[p] = c
	}
	out[path] = content
	return out
}

// A followed branch that moves upstream, for a model and a dataset: a sync
// fetches only the file that changed, moves the branch, keeps the earlier
// revision resolvable by its id, and a second sync does nothing.
func TestSyncMovesAFollowedBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, typ := range []registry.RepoType{registry.TypeModel, registry.TypeDataset} {
		t.Run(string(typ), func(t *testing.T) {
			t.Parallel()
			hub := newSyncHub(t)
			key := registry.RepoKey(typ, "acme/tiny")
			first := push(t, hub.inst, key, modelFiles, client.PushOptions{})
			local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url})
			mirrorMain(t, local, key)

			resp := follow(t, local, key, `{}`)
			var entry struct {
				Name   string            `json:"name"`
				Type   registry.RepoType `json:"type"`
				Branch string            `json:"branch"`
			}
			decode(t, resp, &entry)
			if resp.StatusCode != http.StatusOK || entry.Branch != "main" || entry.Name != "acme/tiny" || (typ == registry.TypeDataset) != (entry.Type == registry.TypeDataset) {
				t.Fatalf("follow: status %d, %+v", resp.StatusCode, entry)
			}

			before := hub.files.Load()
			second := push(t, hub.inst, key, with("config.json", `{"model_type":"tiny","v":2}`), client.PushOptions{})
			status, d := syncNow(t, local, key)
			if status != http.StatusOK || !d.Changed || d.outcome("main") != "mirrored" || d.Refs[0].Previous != first.SHA || d.Refs[0].SHA != second.SHA {
				t.Fatalf("sync: status %d, %+v", status, d)
			}
			if got := hub.files.Load() - before; got != 1 {
				t.Fatalf("%d files downloaded by the sync, want only the one that changed", got)
			}
			head, err := local.Reg.Resolve(ctx, key, "main")
			if err != nil || head.SHA != second.SHA {
				t.Fatalf("main = %+v, %v; want %s", head, err, second.SHA)
			}
			old, err := local.Reg.Resolve(ctx, key, first.SHA)
			if err != nil {
				t.Fatalf("the earlier revision is gone: %v", err)
			}
			if f, err := local.Reg.File(ctx, old, "config.json"); err != nil || f.SHA256 != kit.SHA256Hex(modelFiles["config.json"]) {
				t.Fatalf("the earlier revision's config.json: %+v, %v", f, err)
			}
			if f := followEntry(t, local, key); f.LastChange.IsZero() || f.LastSuccess.IsZero() || f.LastError != "" {
				t.Fatalf("entry after the sync: %+v", f)
			}

			files := hub.files.Load()
			status, d = syncNow(t, local, key)
			if status != http.StatusOK || d.Changed || d.outcome("main") != "unchanged" || hub.files.Load() != files {
				t.Fatalf("second sync: status %d, %+v, %d more downloads", status, d, hub.files.Load()-files)
			}

			var list struct {
				Follows []struct {
					Name       string            `json:"name"`
					Type       registry.RepoType `json:"type"`
					LastCheck  time.Time         `json:"last_check"`
					LastChange time.Time         `json:"last_change"`
				} `json:"follows"`
				Interval int64 `json:"interval_seconds"`
			}
			decode(t, do(t, http.MethodGet, local.URL+"/api/v1/follows", "", nil, nil), &list)
			if len(list.Follows) != 1 || list.Follows[0].LastCheck.IsZero() || list.Follows[0].LastChange.IsZero() || list.Interval != 0 {
				t.Fatalf("follow list %+v", list)
			}
		})
	}
}

// A repository first fetched by pull-through can be followed, and after a
// sync the new revision is stored in full, not listed.
func TestSyncAPulledThroughRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newSyncHub(t)
	push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	if resp := do(t, http.MethodGet, local.URL+"/acme/tiny/resolve/main/config.json", "", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("pull-through: %d", resp.StatusCode)
	}
	mustFollow(t, local, "acme/tiny", `{}`)
	second := push(t, hub.inst, "acme/tiny", with("model.safetensors", strings.Repeat("new weights", 3000)), client.PushOptions{})
	if status, d := syncNow(t, local, "acme/tiny"); status != http.StatusOK || !d.Changed {
		t.Fatalf("sync: %d, %+v", status, d)
	}
	head, err := local.Reg.Resolve(ctx, "acme/tiny", "main")
	if err != nil || head.SHA != second.SHA {
		t.Fatalf("main = %+v, %v", head, err)
	}
	if n, err := local.Reg.PendingCount(ctx, head); err != nil || n != 0 {
		t.Fatalf("%d files of the synced revision not stored (%v)", n, err)
	}
}

// Only what came from an upstream can be followed.
func TestFollowRefusesWhatWasCreatedHere(t *testing.T) {
	t.Parallel()
	local := regtest.Start(t, server.Config{AnonymousRead: true})
	push(t, local, "acme/mine", modelFiles, client.PushOptions{})
	push(t, local, "datasets/acme/mine", modelFiles, client.PushOptions{})
	for _, key := range []string{"acme/mine", "datasets/acme/mine"} {
		resp := follow(t, local, key, `{"branch":"main"}`)
		if msg := body(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(msg, "mirroring or pull-through") {
			t.Fatalf("following the pushed %s: %d %s", key, resp.StatusCode, msg)
		}
	}
	if resp := follow(t, local, "acme/absent", `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("following a repository not held: %d", resp.StatusCode)
	}
	if status, _ := syncNow(t, local, "acme/mine"); status != http.StatusNotFound {
		t.Fatalf("syncing a repository not followed: %d", status)
	}
	if resp := do(t, http.MethodDelete, local.URL+"/api/v1/repos/acme/mine/follow", local.Write, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unfollowing a repository not followed: %d", resp.StatusCode)
	}
}

// What a sync never does: move a tag, or delete a ref the upstream no
// longer has. And what a failure is: a followed branch gone upstream, and
// content that does not verify, each recorded against the repository with
// the branch left where it was.
func TestSyncTagsDeletionsAndFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newSyncHub(t)
	first := push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{Tags: []string{"v1"}})
	dev := push(t, hub.inst, "acme/tiny", with("config.json", `{"dev":true}`), client.PushOptions{Branch: "dev"})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url})
	mirrorMain(t, local, "acme/tiny")

	// Every ref: the tag is recorded as a tag.
	mustFollow(t, local, "acme/tiny", `{"all_refs":true}`)
	status, d := syncNow(t, local, "acme/tiny")
	if status != http.StatusOK || d.outcome("v1") != "mirrored" || d.outcome("dev") != "mirrored" || d.outcome("main") != "unchanged" {
		t.Fatalf("first sync: %d, %+v", status, d)
	}
	if ref, err := local.Reg.Ref(ctx, "acme/tiny", "v1"); err != nil || ref.Kind != registry.KindTag || ref.SHA != first.SHA {
		t.Fatalf("v1 = %+v, %v", ref, err)
	}

	// The upstream moves main, and its tag with it; and deletes dev.
	second := push(t, hub.inst, "acme/tiny", with("config.json", `{"v":2}`), client.PushOptions{})
	hub.fakeRefs("acme/tiny", map[string]string{"main": second.SHA}, map[string]string{"v1": second.SHA})
	status, d = syncNow(t, local, "acme/tiny")
	if status != http.StatusOK || d.outcome("main") != "mirrored" || d.outcome("v1") != "tag_moved" || d.outcome("dev") != "deleted_upstream" {
		t.Fatalf("sync after the tag moved: %d, %+v", status, d)
	}
	for ref, want := range map[string]string{"v1": first.SHA, "dev": dev.SHA, "main": second.SHA} {
		if got, err := local.Reg.Ref(ctx, "acme/tiny", ref); err != nil || got.SHA != want {
			t.Fatalf("%s = %+v, %v; want it at %s", ref, got, err, want)
		}
	}
	if f := followEntry(t, local, "acme/tiny"); f.LastError != "" || f.Failures != 0 {
		t.Fatalf("a moved tag was recorded as a failure: %+v", f)
	}

	// Following dev alone, which the upstream no longer has, fails.
	mustFollow(t, local, "acme/tiny", `{"branch":"dev"}`)
	status, d = syncNow(t, local, "acme/tiny")
	if status != http.StatusBadGateway || !strings.Contains(d.Error, `"dev"`) || d.outcome("dev") != "deleted_upstream" {
		t.Fatalf("sync of a branch deleted upstream: %d, %+v", status, d)
	}
	if f := followEntry(t, local, "acme/tiny"); !strings.Contains(f.LastError, "dev") || f.Failures != 1 || f.LastErrorAt.IsZero() {
		t.Fatalf("entry after the failure: %+v", f)
	}
	if got, _ := local.Reg.Ref(ctx, "acme/tiny", "dev"); got.SHA != dev.SHA {
		t.Fatalf("dev moved to %s", got.SHA)
	}

	// Content that does not verify: nothing recorded, main stays.
	hub.fakeRefs("acme/tiny", nil, nil)
	mustFollow(t, local, "acme/tiny", `{"branch":"main"}`)
	push(t, hub.inst, "acme/tiny", with("model.safetensors", strings.Repeat("other weights", 3000)), client.PushOptions{})
	hub.tampered.Store(true)
	status, d = syncNow(t, local, "acme/tiny")
	if status != http.StatusBadGateway || d.Error == "" || d.outcome("main") != "failed" {
		t.Fatalf("sync of content that does not verify: %d, %+v", status, d)
	}
	if head, _ := local.Reg.Resolve(ctx, "acme/tiny", "main"); head.SHA != second.SHA {
		t.Fatalf("main moved to %s after a failed verification", head.SHA)
	}
	if f := followEntry(t, local, "acme/tiny"); f.Failures != 2 || f.LastError == "" {
		t.Fatalf("entry after the verification failure: %+v", f)
	}

	text := body(t, do(t, http.MethodGet, local.URL+"/metrics", "", nil, nil))
	for _, line := range []string{
		`modelregistry_sync_failures_total{outcome="verification"} 1`,
		`modelregistry_sync_failures_total{outcome="not_found"} 1`,
		`modelregistry_sync_checks_total 4`,
		`modelregistry_sync_changes_total 2`,
		`modelregistry_followed_repositories 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("metrics lack %q", line)
		}
	}
}

// checkLog is a log handler that hands the repository of every "followed
// repository checked" line to whoever reads the channel, and waits until
// it is read. Other lines are dropped.
type checkLog chan string

func (c checkLog) Enabled(context.Context, slog.Level) bool { return true }
func (c checkLog) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c checkLog) WithGroup(string) slog.Handler            { return c }
func (c checkLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "followed repository checked" {
		return nil
	}
	repo := ""
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "repo" {
			repo = a.Value.String()
		}
		return true
	})
	c <- repo
	return nil
}

// next reads the repositories of n checks, in order of name.
func (c checkLog) next(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for range n {
		select {
		case repo := <-c:
			out = append(out, repo)
		case <-time.After(30 * time.Second):
			t.Fatalf("waited for %d checks, saw %v", n, out)
		}
	}
	sort.Strings(out)
	return out
}

// testClock is a clock the test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// The schedule, driven by the test's ticks and clock: every repository is
// checked within the first interval; one whose upstream fails is checked
// less often while the other goes on, and recovers when its upstream does.
func TestScheduledSyncBacksOffAFailingUpstream(t *testing.T) {
	t.Parallel()
	hub := newSyncHub(t)
	push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{})
	push(t, hub.inst, "acme/other", modelFiles, client.PushOptions{})
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &testClock{t: t0}
	ticks := make(chan time.Time)
	checked := make(checkLog)
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, Now: clock.Now,
		SyncInterval: time.Hour, SyncTicks: ticks, Logger: slog.New(checked)})
	for _, key := range []string{"acme/tiny", "acme/other"} {
		mirrorMain(t, local, key)
		mustFollow(t, local, key, `{}`)
	}
	hub.setDown("acme/other", true)
	tick := func(at time.Time) {
		clock.set(at)
		ticks <- at
	}

	// Nothing is due before a repository's place in the first interval;
	// at the end of it, both are.
	tick(t0.Add(time.Hour))
	if got := checked.next(t, 2); strings.Join(got, " ") != "acme/other acme/tiny" {
		t.Fatalf("first interval checked %v", got)
	}
	other := followEntry(t, local, "acme/other")
	if other.Failures != 1 || !strings.Contains(other.LastError, "503") || !other.LastCheck.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the failing repository: %+v", other)
	}
	if tiny := followEntry(t, local, "acme/tiny"); tiny.Failures != 0 || !tiny.LastSuccess.Equal(t0.Add(time.Hour)) {
		t.Fatalf("the other repository: %+v", tiny)
	}

	// An interval later only the healthy one is due: the failing one waits
	// twice as long.
	tick(t0.Add(2 * time.Hour))
	if got := checked.next(t, 1); got[0] != "acme/tiny" {
		t.Fatalf("second interval checked %v", got)
	}

	// Its upstream is back. When it is due again it is checked, and
	// succeeds.
	hub.setDown("acme/other", false)
	tick(t0.Add(3 * time.Hour))
	if got := checked.next(t, 2); strings.Join(got, " ") != "acme/other acme/tiny" {
		t.Fatalf("third interval checked %v", got)
	}
	other = followEntry(t, local, "acme/other")
	if other.Failures != 0 || other.LastError != "" || !other.LastCheck.Equal(t0.Add(3*time.Hour)) || !other.LastSuccess.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("the recovered repository: %+v", other)
	}
	var list struct {
		Follows []struct {
			Name      string    `json:"name"`
			NextCheck time.Time `json:"next_check"`
		} `json:"follows"`
		Interval int64 `json:"interval_seconds"`
	}
	decode(t, do(t, http.MethodGet, local.URL+"/api/v1/follows", "", nil, nil), &list)
	if list.Interval != 3600 || len(list.Follows) != 2 || !list.Follows[0].NextCheck.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("follow list %+v", list)
	}
}

// The follow list survives a restart, and the schedule of the new process
// checks what it lists.
func TestFollowListSurvivesARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newSyncHub(t)
	push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{})
	first := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url})
	mirrorMain(t, first, "acme/tiny")
	mustFollow(t, first, "acme/tiny", `{}`)
	first.StopServing()
	if err := first.Reg.Close(); err != nil {
		t.Fatal(err)
	}

	second := push(t, hub.inst, "acme/tiny", with("config.json", `{"v":2}`), client.PushOptions{})
	reg, err := registry.Open(first.Data, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time)
	checked := make(checkLog)
	clock := &testClock{t: t0}
	regtest.Serve(t, reg, server.Config{AnonymousRead: true, Upstream: hub.url, Now: clock.Now,
		SyncInterval: time.Hour, SyncTicks: ticks, Logger: slog.New(checked)})
	clock.set(t0.Add(time.Hour))
	ticks <- t0.Add(time.Hour)
	if got := checked.next(t, 1); got[0] != "acme/tiny" {
		t.Fatalf("checked %v", got)
	}
	if head, err := reg.Resolve(ctx, "acme/tiny", "main"); err != nil || head.SHA != second.SHA {
		t.Fatalf("main after the restart's sync: %+v, %v", head, err)
	}
}

// Stopping the server during a sync cancels it and waits for it: the client
// that asked is told to come back, and nothing is recorded.
func TestShutdownStopsASync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newSyncHub(t)
	first := push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url})
	mirrorMain(t, local, "acme/tiny")
	mustFollow(t, local, "acme/tiny", `{}`)
	push(t, hub.inst, "acme/tiny", with("model.safetensors", strings.Repeat("held", 9000)), client.PushOptions{})
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	hub.mu.Lock()
	hub.hold = hold
	hub.mu.Unlock()

	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, local.URL+"/api/v1/repos/acme/tiny/sync", nil)
		req.Header.Set("Authorization", "Bearer "+local.Write)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	select {
	case <-hub.fetching:
	case <-time.After(30 * time.Second):
		t.Fatal("the sync never began fetching")
	}
	local.StopServing() // fails the test if the sync is not stopped and waited for
	if status := <-answered; status != http.StatusServiceUnavailable {
		t.Fatalf("the client waiting on the sync was answered %d, want 503", status)
	}
	if f := followEntry(t, local, "acme/tiny"); !f.LastCheck.IsZero() || f.LastError != "" {
		t.Fatalf("a sync cut short was recorded: %+v", f)
	}
	if head, _ := local.Reg.Resolve(ctx, "acme/tiny", "main"); head.SHA != first.SHA {
		t.Fatalf("main moved to %s", head.SHA)
	}
}

// Mirroring every ref through the API: one ref that fails is reported on
// its own line and in the last, and does not undo the others.
func TestMirrorAllRefsWithOneFailing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newSyncHub(t)
	main := push(t, hub.inst, "acme/tiny", modelFiles, client.PushOptions{Tags: []string{"v1"}})
	missing := strings.Repeat("e", 40)
	hub.fakeRefs("acme/tiny", map[string]string{"main": main.SHA, "broken": missing}, map[string]string{"v1": main.SHA})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url})

	resp := do(t, http.MethodPost, local.URL+"/api/v1/mirror", local.Write, strings.NewReader(`{"repo":"acme/tiny","all_refs":true}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body(t, resp))
	}
	var lines []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	last := lines[len(lines)-1]
	if last["event"] != "done" {
		t.Fatalf("last line %v", last)
	}
	outcomes := map[string]string{}
	for _, r := range last["refs"].([]any) {
		ref := r.(map[string]any)
		outcomes[ref["name"].(string)] = ref["outcome"].(string)
	}
	if fmt.Sprint(outcomes) != "map[broken:failed main:mirrored v1:mirrored]" {
		t.Fatalf("outcomes %v", outcomes)
	}
	refLines := 0
	for _, l := range lines {
		if l["event"] == "ref" {
			refLines++
		}
	}
	if refLines != 3 {
		t.Fatalf("%d ref lines", refLines)
	}
	if ref, err := local.Reg.Ref(ctx, "acme/tiny", "v1"); err != nil || ref.Kind != registry.KindTag {
		t.Fatalf("v1 = %+v, %v", ref, err)
	}
	if _, err := local.Reg.Ref(ctx, "acme/tiny", "broken"); err == nil {
		t.Fatal("the failed ref was recorded")
	}

	// Naming a revision as well is refused.
	resp = do(t, http.MethodPost, local.URL+"/api/v1/mirror", local.Write, strings.NewReader(`{"repo":"acme/tiny","all_refs":true,"revision":"main"}`), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("all_refs with a revision: %d", resp.StatusCode)
	}
}
