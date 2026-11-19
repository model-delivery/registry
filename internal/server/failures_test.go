// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// serverPath is part of every error the failing stores below return. No
// response may contain it: it stands for the layout of the server's disk.
const serverPath = "/srv/registry"

// fullDisk is a store that cannot stage anything while full is set.
type fullDisk struct {
	blob.Store
	full atomic.Bool
}

func (d *fullDisk) Stage(ctx context.Context, r io.Reader, want blob.Expect) (blob.Staged, error) {
	if d.full.Load() {
		return nil, errors.New("create staging file: open " + serverPath + "/tmp/stage-1: no space left on device")
	}
	return d.Store.Stage(ctx, r, want)
}

// stuckFiles is a store that cannot remove a blob.
type stuckFiles struct{ blob.Store }

func (stuckFiles) Delete(_ context.Context, digest string) error {
	return errors.New("remove " + serverPath + "/blobs/sha256/" + digest + ": permission denied")
}

func newFS(t *testing.T) blob.Store {
	t.Helper()
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// signedInForm is the headers of a form posted by a browser that signed in
// with token.
func signedInForm(token string) map[string]string {
	return map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Cookie": "md_token=" + token}
}

// mirrorLines posts a mirror request and returns the lines of its answer.
func mirrorLines(t *testing.T, in *regtest.Instance, repo string) (status int, lines []map[string]any) {
	t.Helper()
	resp := do(t, http.MethodPost, in.URL+"/api/v1/mirror", in.Write, jsonBody(t, map[string]any{"repo": repo}), nil)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("mirror answered a line that is not JSON: %s", sc.Text())
		}
		lines = append(lines, line)
	}
	return resp.StatusCode, lines
}

// A failure of the registry's own while it fetches from the upstream (here
// a full disk) is a 500 with nothing of the failure in it. It used to be
// answered 502 "could not fetch from upstream", with the path of the file
// the server could not create. A failure the upstream caused is still 502.
func TestLocalFailuresAreNotReportedAsUpstreamFailures(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	disk := &fullDisk{Store: newFS(t)}
	logs := &regtest.LogBuffer{}
	local := regtest.StartWith(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true,
		Logger: slog.New(slog.NewTextHandler(logs, nil))}, disk)
	const file = "/acme/tiny/resolve/main/model.safetensors"

	t.Run("pull-through of a file", func(t *testing.T) {
		disk.full.Store(true)
		resp := do(t, http.MethodGet, local.URL+file, "", nil, nil)
		shown := body(t, resp) + resp.Header.Get("X-Error-Message")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500: %s", resp.StatusCode, shown)
		}
		for _, leak := range []string{serverPath, "stage-1", "no space", "upstream"} {
			if strings.Contains(shown, leak) {
				t.Errorf("the answer mentions %q: %s", leak, shown)
			}
		}
		if got := logs.String(); !strings.Contains(got, serverPath+"/tmp/stage-1") || !strings.Contains(got, "level=ERROR") {
			t.Errorf("the failure was not logged in full:\n%s", got)
		}
		// Nothing was recorded, so the file is fetched once the disk has room.
		disk.full.Store(false)
		resp = do(t, http.MethodGet, local.URL+file, "", nil, nil)
		if resp.StatusCode != http.StatusOK || body(t, resp) != modelFiles["model.safetensors"] {
			t.Fatalf("after the disk had room again: status %d", resp.StatusCode)
		}
	})

	t.Run("an upstream failure is still a gateway error with its reason", func(t *testing.T) {
		hub.tampered.Store(true)
		defer hub.tampered.Store(false)
		fresh := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
		resp := do(t, http.MethodGet, fresh.URL+file, "", nil, nil)
		msg := body(t, resp)
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "could not fetch from upstream") || !strings.Contains(msg, "verification failed") {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		if resp.Header.Get("X-Error-Message") == "" {
			t.Error("no X-Error-Message: a Hugging Face client would show a bare status code")
		}
	})

	t.Run("mirror", func(t *testing.T) {
		target := &fullDisk{Store: newFS(t)}
		target.full.Store(true)
		mirrorLogs := &regtest.LogBuffer{}
		in := regtest.StartWith(t, server.Config{AnonymousRead: true, Upstream: hub.url,
			Logger: slog.New(slog.NewTextHandler(mirrorLogs, nil))}, target)

		status, lines := mirrorLines(t, in, "acme/tiny")
		if status != http.StatusOK || len(lines) == 0 {
			t.Fatalf("status %d with %d lines", status, len(lines))
		}
		last := lines[len(lines)-1]
		reason, _ := last["error"].(string)
		if last["event"] != "error" || reason == "" {
			t.Fatalf("last line = %v, want an error line", last)
		}
		for _, leak := range []string{serverPath, "stage-1", "no space"} {
			if strings.Contains(reason, leak) {
				t.Errorf("the error line mentions %q: %s", leak, reason)
			}
		}
		if !strings.Contains(reason, "server log") {
			t.Errorf("the error line does not say where the reason is: %s", reason)
		}
		if got := mirrorLogs.String(); !strings.Contains(got, serverPath+"/tmp/stage-1") || !strings.Contains(got, "level=ERROR") {
			t.Errorf("the failure was not logged in full:\n%s", got)
		}
		if _, err := in.Reg.GetRepo(context.Background(), "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
			t.Errorf("a failed mirror recorded something: %v", err)
		}

		// The upstream's failure keeps its reason on the same line.
		target.full.Store(false)
		hub.tampered.Store(true)
		defer hub.tampered.Store(false)
		_, lines = mirrorLines(t, in, "acme/tiny")
		last = lines[len(lines)-1]
		if reason, _ := last["error"].(string); last["event"] != "error" || !strings.Contains(reason, "verification failed") {
			t.Fatalf("last line = %v, want the verification failure", last)
		}
	})
}

// A repository whose deletion committed is reported as deleted, even when
// its files could not then be removed. It used to be "500 internal error"
// for a repository that was in fact gone.
func TestDeletingARepositoryWhoseFilesCannotBeRemoved(t *testing.T) {
	t.Parallel()
	type answer struct {
		Blobs   *int    `json:"blobs"`
		Bytes   *int64  `json:"bytes"`
		Warning *string `json:"warning"`
		Error   string  `json:"error"`
	}
	remove := func(t *testing.T, in *regtest.Instance) (int, answer, string) {
		t.Helper()
		resp := do(t, http.MethodDelete, in.URL+"/api/v1/repos/acme/tiny", in.Admin, nil, nil)
		raw := body(t, resp)
		var out answer
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		return resp.StatusCode, out, raw
	}

	t.Run("registry API", func(t *testing.T) {
		t.Parallel()
		logs := &regtest.LogBuffer{}
		in := regtest.StartWith(t, server.Config{AnonymousRead: true, Logger: slog.New(slog.NewTextHandler(logs, nil))}, stuckFiles{newFS(t)})
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})

		status, out, raw := remove(t, in)
		if status != http.StatusOK {
			t.Fatalf("a repository that was deleted was answered %d: %s", status, raw)
		}
		if out.Blobs == nil || out.Bytes == nil {
			t.Fatalf("the answer lost its counts: %s", raw)
		}
		if out.Warning == nil || !strings.Contains(*out.Warning, "was deleted") || !strings.Contains(*out.Warning, "could not be removed") {
			t.Fatalf("warning = %v, want it to say the repository is gone and files are not: %s", out.Warning, raw)
		}
		if strings.Contains(raw, serverPath) || strings.Contains(raw, "permission denied") {
			t.Fatalf("the answer carries the server's reason: %s", raw)
		}
		if got := logs.String(); !strings.Contains(got, "permission denied") || !strings.Contains(got, "level=WARN") {
			t.Fatalf("the reason was not logged:\n%s", got)
		}
		// Nothing was removed, so nothing is counted as freed, each file has
		// its own line in the log, and the registry still knows every one of
		// them: a later garbage collection finds them again.
		if *out.Blobs != 0 || *out.Bytes != 0 {
			t.Fatalf("the answer counts files that were not removed: %s", raw)
		}
		if n := strings.Count(logs.String(), "stored files could not be removed"); n != len(modelFiles) {
			t.Fatalf("%d of the %d files that could not be removed were logged:\n%s", n, len(modelFiles), logs.String())
		}
		if st, err := in.Reg.Stats(context.Background()); err != nil || st.Blobs != len(modelFiles) {
			t.Fatalf("stats = %+v, %v; want the %d files still recorded", st, err, len(modelFiles))
		}
		if resp := do(t, http.MethodGet, in.URL+"/api/v1/repos/acme/tiny", "", nil, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("the repository is still there: %d", resp.StatusCode)
		}
		// Deleting it again is the ordinary "no such repository".
		if status, _, raw := remove(t, in); status != http.StatusNotFound {
			t.Fatalf("second delete: %d %s", status, raw)
		}
	})

	t.Run("no warning when everything was removed", func(t *testing.T) {
		t.Parallel()
		in := regtest.Start(t, server.Config{AnonymousRead: true})
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
		status, out, raw := remove(t, in)
		if status != http.StatusOK || out.Warning != nil || strings.Contains(raw, "warning") {
			t.Fatalf("status %d: %s; want the counts and no warning field", status, raw)
		}
		if out.Blobs == nil || *out.Blobs != len(modelFiles) {
			t.Fatalf("blobs = %v, want %d: %s", out.Blobs, len(modelFiles), raw)
		}
	})

	t.Run("web UI", func(t *testing.T) {
		t.Parallel()
		in := regtest.StartWith(t, server.Config{AnonymousRead: true}, stuckFiles{newFS(t)})
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
		resp := do(t, http.MethodPost, in.URL+"/ui/repo/acme/tiny/delete", "", strings.NewReader("confirm=acme%2Ftiny"), signedInForm(in.Admin))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?notice=deleted-partly" {
			t.Fatalf("status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		page := body(t, do(t, http.MethodGet, in.URL+resp.Header.Get("Location"), "", nil, nil))
		if !strings.Contains(page, "The repository was deleted, but") || !strings.Contains(page, "could not be removed") || strings.Contains(page, serverPath) {
			t.Fatalf("the page after the delete does not say what happened, or says too much:\n%s", page)
		}
		if _, err := in.Reg.GetRepo(context.Background(), "acme/tiny"); !errors.Is(err, registry.ErrRepoNotFound) {
			t.Fatalf("the repository is still there: %v", err)
		}
	})

	t.Run("the notice after a clean delete allows for an upload in flight", func(t *testing.T) {
		t.Parallel()
		in := regtest.Start(t, server.Config{AnonymousRead: true})
		page := body(t, do(t, http.MethodGet, in.URL+"/?notice=deleted", "", nil, nil))
		if !strings.Contains(page, "The repository was deleted") || !strings.Contains(page, "upload in progress") {
			t.Fatalf("notice does not mention files spared for an upload:\n%s", page)
		}
	})
}

// Every error on a Hugging Face route has its message in X-Error-Message
// as well as in the body, and its code in X-Error-Code when it has one.
// huggingface_hub shows the first and picks its exception by the second.
func TestHuggingFaceErrorsAlwaysCarryTheirMessage(t *testing.T) {
	t.Parallel()
	open := regtest.Start(t, server.Config{AnonymousRead: true})
	private := regtest.Start(t, server.Config{AnonymousRead: false})
	for _, in := range []*regtest.Instance{open, private} {
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	}
	sha40 := strings.Repeat("0", 40)

	tests := []struct {
		name   string
		in     *regtest.Instance
		method string
		path   string
		token  string
		body   string
		status int
		code   string
	}{
		{"unknown repository", open, http.MethodGet, "/api/models/acme/absent/revision/main", "", "", 404, "RepoNotFound"},
		{"unknown revision", open, http.MethodGet, "/api/models/acme/tiny/revision/nope", "", "", 404, "RevisionNotFound"},
		{"unknown file", open, http.MethodGet, "/acme/tiny/resolve/main/absent.txt", "", "", 404, "EntryNotFound"},
		{"unknown directory", open, http.MethodGet, "/api/models/acme/tiny/tree/main/nowhere", "", "", 404, "EntryNotFound"},
		{"unknown repository's refs", open, http.MethodGet, "/api/models/acme/absent/refs", "", "", 404, "RepoNotFound"},
		{"not a namespace/name id", open, http.MethodGet, "/api/models/single", "", "", 404, ""},
		{"route not implemented", open, http.MethodGet, "/api/models/acme/tiny/discussions", "", "", 404, ""},
		{"method not offered", open, http.MethodDelete, "/api/models/acme/tiny", "", "", 405, ""},
		{"method not offered on a file", open, http.MethodDelete, "/acme/tiny/resolve/main/config.json", "", "", 405, ""},
		{"anonymous read of a private registry", private, http.MethodGet, "/api/models/acme/tiny", "", "", 401, ""},
		{"anonymous download from a private registry", private, http.MethodGet, "/acme/tiny/resolve/main/config.json", "", "", 401, ""},
		{"anonymous upload", open, http.MethodPost, "/api/models/acme/tiny/preupload/main", "", `{}`, 401, ""},
		{"upload with a read token", open, http.MethodPost, "/api/models/acme/tiny/commit/main", open.Read, `{}`, 403, ""},
		{"whoami without a token", open, http.MethodGet, "/api/whoami-v2", "", "", 401, ""},
		{"repository that exists", open, http.MethodPost, "/api/repos/create", open.Write, `{"name":"tiny","organization":"acme"}`, 409, ""},
		{"repository id that is not one", open, http.MethodPost, "/api/repos/create", open.Write, `{"name":"a b","organization":"acme"}`, 400, ""},
		{"commit to a revision id", open, http.MethodPost, "/api/models/acme/tiny/commit/" + sha40, open.Write, `{}`, 400, ""},
		{"commit with an unknown operation", open, http.MethodPost, "/api/models/acme/tiny/commit/main", open.Write, `{"key":"rename","value":{}}`, 400, ""},
		{"branch in an unknown repository", open, http.MethodPost, "/api/models/acme/absent/branch/dev", open.Write, `{}`, 404, "RepoNotFound"},
		{"branch from an unknown revision", open, http.MethodPost, "/api/models/acme/tiny/branch/dev", open.Write, `{"startingPoint":"nope"}`, 404, "RevisionNotFound"},
		{"large-file batch for a download", open, http.MethodPost, "/acme/tiny.git/info/lfs/objects/batch", open.Write, `{"operation":"download"}`, 501, ""},
		{"large-file upload without a signed URL", open, http.MethodPut, "/api/lfs/upload/" + kit.SHA256Hex("x"), "", "x", 403, ""},
		{"xet", open, http.MethodGet, "/api/xet-is-not-supported/x", "", "", 400, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rd io.Reader
			if tc.body != "" {
				rd = strings.NewReader(tc.body)
			}
			resp := do(t, tc.method, tc.in.URL+tc.path, tc.token, rd, nil)
			raw := body(t, resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.status, raw)
			}
			var out struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Error == "" {
				t.Fatalf("body is not a JSON error: %s", raw)
			}
			if got := resp.Header.Get("X-Error-Message"); got != out.Error {
				t.Errorf("X-Error-Message = %q, body says %q", got, out.Error)
			}
			if got := resp.Header.Get("X-Error-Code"); got != tc.code {
				t.Errorf("X-Error-Code = %q, want %q", got, tc.code)
			}
		})
	}

	t.Run("an existing repository is named in the conflict", func(t *testing.T) {
		t.Parallel()
		resp := do(t, http.MethodPost, open.URL+"/api/repos/create", open.Write, strings.NewReader(`{"name":"tiny","organization":"acme"}`), nil)
		var out map[string]string
		decode(t, resp, &out)
		if resp.StatusCode != http.StatusConflict || out["url"] != open.URL+"/acme/tiny" || out["error"] == "" || len(out) != 2 {
			t.Fatalf("status %d body %v; want the error and the repository's url, as before", resp.StatusCode, out)
		}
	})
}

// The same refusal is written three ways, by who is asking: the registry
// API's JSON, the Hugging Face header as well, or a page for a browser.
func TestRefusalsAreWrittenForWhoeverAsked(t *testing.T) {
	t.Parallel()
	in := regtest.Start(t, server.Config{AnonymousRead: false})
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})

	t.Run("registry API", func(t *testing.T) {
		t.Parallel()
		for token, want := range map[string]int{"": 401, in.Read: 403} {
			resp := do(t, http.MethodDelete, in.URL+"/api/v1/repos/acme/tiny", token, nil, nil)
			raw := body(t, resp)
			if resp.StatusCode != want || !strings.Contains(raw, `"error"`) || resp.Header.Get("Content-Type") != "application/json" {
				t.Errorf("status %d (%s), want %d as JSON", resp.StatusCode, raw, want)
			}
			if (resp.Header.Get("WWW-Authenticate") != "") != (want == 401) {
				t.Errorf("status %d with WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		}
	})

	t.Run("Hugging Face", func(t *testing.T) {
		t.Parallel()
		for token, want := range map[string]int{"": 401, in.Read: 403} {
			resp := do(t, http.MethodPost, in.URL+"/api/models/acme/tiny/commit/main", token, strings.NewReader(`{}`), nil)
			msg := resp.Header.Get("X-Error-Message")
			if resp.StatusCode != want || !strings.Contains(msg, "write role") {
				t.Errorf("status %d with X-Error-Message %q, want %d naming the role needed", resp.StatusCode, msg, want)
			}
			if (resp.Header.Get("WWW-Authenticate") != "") != (want == 401) {
				t.Errorf("status %d with WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		}
	})

	t.Run("web UI", func(t *testing.T) {
		t.Parallel()
		// May not read: sent to sign in.
		resp := do(t, http.MethodGet, in.URL+"/ui/repo/acme/tiny", "", nil, nil)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/login" {
			t.Errorf("anonymous page view: status %d to %q", resp.StatusCode, resp.Header.Get("Location"))
		}
		// May read, may not delete: shown the sign-in page with the reason.
		resp = do(t, http.MethodPost, in.URL+"/ui/repo/acme/tiny/delete", "", strings.NewReader("confirm=acme%2Ftiny"), signedInForm(in.Write))
		page := body(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, "admin role") || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("delete with a write token: status %d\n%s", resp.StatusCode, page)
		}
		if _, err := in.Reg.GetRepo(context.Background(), "acme/tiny"); err != nil {
			t.Errorf("the refused delete removed the repository: %v", err)
		}
	})
}

// The snapshot has a consistent address beside the other backup routes,
// and the one v0.3.0 shipped still works.
func TestSnapshotAddresses(t *testing.T) {
	t.Parallel()
	in := regtest.Start(t, server.Config{AnonymousRead: true})
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	for _, path := range []string{"/api/v1/backups/snapshot", "/api/v1/backup/snapshot"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			for token, want := range map[string]int{"": 401, in.Read: 403, in.Write: 403} {
				if resp := do(t, http.MethodGet, in.URL+path, token, nil, nil); resp.StatusCode != want {
					t.Errorf("status %d, want %d", resp.StatusCode, want)
				}
			}
			resp := do(t, http.MethodGet, in.URL+path, in.Admin, nil, nil)
			content := body(t, resp)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/vnd.sqlite3" || !strings.HasPrefix(content, "SQLite format 3") {
				t.Fatalf("status %d, type %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(content))
			}
			if !strings.Contains(resp.Header.Get("Content-Disposition"), ".db") {
				t.Errorf("Content-Disposition = %q", resp.Header.Get("Content-Disposition"))
			}
		})
	}
}
