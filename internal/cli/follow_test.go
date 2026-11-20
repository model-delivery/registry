// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
)

// mirror --all-refs and --follow, follow list and stop, and sync, against
// a registry playing the upstream: a ref that fails is reported, fails the
// command and does not undo the others; following and syncing work for a
// model and a dataset.
func TestMirrorAllRefsFollowAndSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hub := newFixture(t, "")
	src := modelDir(t)
	if code, _, errOut := hub.run(t, hub.admin, "push", src, "acme/tiny", "--tag", "v1"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := hub.run(t, hub.admin, "push", src, "acme/table", "--repo-type", "dataset"); code != 0 {
		t.Fatal(errOut)
	}
	head, err := hub.reg.Resolve(ctx, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}

	// In front of the upstream: while broken is set, its refs of acme/tiny
	// list one more branch, at a revision it does not have.
	var broken atomic.Bool
	target, _ := url.Parse(hub.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() && r.URL.Path == "/api/models/acme/tiny/refs" {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"branches":[{"name":"main","targetCommit":%q},{"name":"broken","targetCommit":%q}],"tags":[{"name":"v1","targetCommit":%q}],"converts":[]}`,
				head.SHA, strings.Repeat("e", 40), head.SHA))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	f := newFixture(t, front.URL)

	broken.Store(true)
	code, out, errOut := f.run(t, f.admin, "mirror", "acme/tiny", "--all-refs")
	if code != 1 || !strings.Contains(errOut, "Failed: branch broken") || !strings.Contains(errOut, "1 of 3 refs could not be mirrored (broken)") {
		t.Fatalf("mirror with a failing ref: code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "2 mirrored, 1 failed") {
		t.Fatalf("summary:\n%s", out)
	}
	if ref, err := f.reg.Ref(ctx, "acme/tiny", "v1"); err != nil || ref.Kind != registry.KindTag {
		t.Fatalf("v1 after a run where another ref failed: %+v, %v", ref, err)
	}

	broken.Store(false)
	code, out, errOut = f.run(t, f.admin, "mirror", "acme/tiny", "--all-refs", "--follow")
	if code != 0 || !strings.Contains(out, "2 unchanged") || !strings.Contains(out, "Following acme/tiny: every branch and tag") {
		t.Fatalf("mirror --all-refs --follow: code %d\n%s\n%s", code, out, errOut)
	}

	// The upstream moves main; a sync brings it in.
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := hub.run(t, hub.admin, "push", src, "acme/tiny"); code != 0 {
		t.Fatal(errOut)
	}
	code, out, errOut = f.run(t, f.admin, "sync", "acme/tiny")
	if code != 0 || !strings.Contains(out, "Checked acme/tiny against the upstream: updated") || !strings.Contains(out, "mirrored") {
		t.Fatalf("sync: code %d\n%s\n%s", code, out, errOut)
	}
	newHead, _ := hub.reg.Resolve(ctx, "acme/tiny", "main")
	if got, _ := f.reg.Resolve(ctx, "acme/tiny", "main"); got.SHA != newHead.SHA {
		t.Fatalf("main after the sync = %s, want %s", got.SHA, newHead.SHA)
	}

	code, out, _ = f.run(t, "", "follow", "list")
	if code != 0 || !strings.Contains(out, "acme/tiny") || !strings.Contains(out, "every branch and tag") || !strings.Contains(out, "no sync schedule") {
		t.Fatalf("follow list: code %d\n%s", code, out)
	}
	code, out, _ = f.run(t, "", "follow", "list", "--json")
	var list struct {
		Follows []struct {
			Name    string `json:"name"`
			AllRefs bool   `json:"all_refs"`
		} `json:"follows"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list.Follows) != 1 || !list.Follows[0].AllRefs {
		t.Fatalf("follow list --json: code %d\n%s", code, out)
	}

	// A dataset, following its branch.
	code, out, errOut = f.run(t, f.admin, "mirror", "acme/table", "--repo-type", "dataset", "--follow")
	if code != 0 || !strings.Contains(out, "Following dataset acme/table: branch main") {
		t.Fatalf("mirror a dataset --follow: code %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = f.run(t, f.admin, "sync", "acme/table", "--repo-type", "dataset")
	if code != 0 || !strings.Contains(out, "Checked dataset acme/table against the upstream: already up to date") {
		t.Fatalf("sync a dataset: code %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = f.run(t, f.admin, "sync", "--all")
	if code != 0 || !strings.Contains(out, "Started a check of 2 followed repositories") {
		t.Fatalf("sync --all: code %d\n%s", code, out)
	}

	code, out, errOut = f.run(t, f.admin, "follow", "stop", "acme/tiny")
	if code != 0 || !strings.Contains(out, "No longer following acme/tiny") {
		t.Fatalf("follow stop: code %d\n%s\n%s", code, out, errOut)
	}
	if code, _, errOut := f.run(t, f.admin, "sync", "acme/tiny"); code != 1 || !strings.Contains(errOut, "not followed") {
		t.Fatalf("sync of a repository no longer followed: code %d\n%s", code, errOut)
	}
}

// What cannot be asked for is refused before any request is made.
func TestMirrorRefsUsage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"mirror", "acme/tiny", "--all-refs", "--revision", "main"}, "leave out --revision"},
		{[]string{"mirror", "acme/tiny", "--follow", "--revision", strings.Repeat("a", 40)}, "a revision id never moves"},
		{[]string{"sync"}, "--all"},
		{[]string{"sync", "acme/tiny", "--all"}, "name none"},
		{[]string{"follow"}, "list or stop"},
		{[]string{"follow", "start"}, "unknown subcommand"},
	}
	for _, tc := range tests {
		code, _, errOut := f.run(t, f.admin, tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: code %d, want 2 with %q:\n%s", tc.args, code, tc.want, errOut)
		}
	}
}
