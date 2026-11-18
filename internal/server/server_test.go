// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// instance is a registry served over HTTP for a test.
type instance struct {
	reg   *registry.Registry
	url   string
	admin string
	read  string
	write string
}

func start(t *testing.T, cfg server.Config) *instance {
	t.Helper()
	return startWith(t, cfg, nil)
}

// startWith is start with a chosen storage backend; nil means local files.
func startWith(t *testing.T, cfg server.Config, store blob.Store) *instance {
	t.Helper()
	reg, err := registry.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := &instance{reg: reg}
	for name, dst := range map[string]*string{registry.RoleAdmin: &in.admin, registry.RoleWrite: &in.write, registry.RoleRead: &in.read} {
		if *dst, err = reg.CreateToken(ctx, name, name); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(server.New(reg, cfg))
	in.url = srv.URL
	t.Cleanup(func() {
		srv.Close()
		_ = reg.Close()
	})
	return in
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

var modelFiles = map[string]string{
	"config.json":         `{"model_type":"tiny"}`,
	"model.safetensors":   strings.Repeat("weights", 5000),
	"tokenizer/vocab.txt": "a\nb\nc\n",
}

func push(t *testing.T, in *instance, repo string, files map[string]string, opt client.PushOptions) client.PushResult {
	t.Helper()
	if opt.Branch == "" {
		opt.Branch = "main"
	}
	res, err := client.New(in.url, in.write).Push(context.Background(), writeTree(t, files), repo, opt)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func do(t *testing.T, method, url, token string, body io.Reader, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	// Redirects are part of what is being tested, so do not follow them.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// TestHuggingFaceDownloadSurface checks the exact responses huggingface_hub
// depends on: revision lookup, file listing, and the resolve headers.
func TestHuggingFaceDownloadSurface(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	pushed := push(t, in, "acme/tiny", modelFiles, client.PushOptions{Tags: []string{"v1"}})

	t.Run("model info names the revision and files", func(t *testing.T) {
		for _, path := range []string{"/api/models/acme/tiny", "/api/models/acme/tiny/revision/main", "/api/models/acme/tiny/revision/v1", "/api/models/acme/tiny/revision/" + pushed.SHA} {
			resp := do(t, http.MethodGet, in.url+path, "", nil, nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d", path, resp.StatusCode)
			}
			var info struct {
				ID       string `json:"id"`
				SHA      string `json:"sha"`
				Siblings []struct {
					Rfilename string `json:"rfilename"`
				} `json:"siblings"`
			}
			decode(t, resp, &info)
			if info.ID != "acme/tiny" || info.SHA != pushed.SHA || len(info.Siblings) != len(modelFiles) {
				t.Fatalf("%s: info = %+v", path, info)
			}
		}
	})

	t.Run("tree lists files with digests and directories", func(t *testing.T) {
		resp := do(t, http.MethodGet, in.url+"/api/models/acme/tiny/tree/"+pushed.SHA+"?recursive=True&expand=False", "", nil, nil)
		var entries []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			OID  string `json:"oid"`
			LFS  *struct {
				OID         string `json:"oid"`
				Size        int64  `json:"size"`
				PointerSize int    `json:"pointerSize"`
			} `json:"lfs"`
		}
		decode(t, resp, &entries)
		got := map[string]string{}
		for _, e := range entries {
			got[e.Path] = e.Type
			if e.Type != "file" {
				continue
			}
			want := modelFiles[e.Path]
			if e.Size != int64(len(want)) || e.LFS == nil || e.LFS.OID != sha(want) || e.LFS.PointerSize == 0 || len(e.OID) != 40 {
				t.Errorf("entry %s = %+v (lfs %+v)", e.Path, e, e.LFS)
			}
		}
		if len(got) != 4 || got["tokenizer"] != "directory" || got["tokenizer/vocab.txt"] != "file" {
			t.Fatalf("tree = %v", got)
		}

		// Without recursive, only the top level is listed.
		resp = do(t, http.MethodGet, in.url+"/api/models/acme/tiny/tree/main", "", nil, nil)
		decode(t, resp, &entries)
		if len(entries) != 3 {
			t.Fatalf("top-level listing has %d entries, want 3", len(entries))
		}
	})

	t.Run("refs list branches and tags", func(t *testing.T) {
		resp := do(t, http.MethodGet, in.url+"/api/models/acme/tiny/refs", "", nil, nil)
		var refs struct {
			Branches, Tags []struct {
				Name, TargetCommit string
			}
		}
		decode(t, resp, &refs)
		if len(refs.Branches) != 1 || refs.Branches[0].Name != "main" || len(refs.Tags) != 1 || refs.Tags[0].TargetCommit != pushed.SHA {
			t.Fatalf("refs = %+v", refs)
		}
	})

	want := modelFiles["model.safetensors"]
	fileURL := in.url + "/acme/tiny/resolve/main/model.safetensors"

	t.Run("HEAD carries etag, size and revision", func(t *testing.T) {
		resp := do(t, http.MethodHead, fileURL, "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("ETag"); got != `"`+sha(want)+`"` {
			t.Errorf("ETag = %s", got)
		}
		if got := resp.Header.Get("X-Repo-Commit"); got != pushed.SHA {
			t.Errorf("X-Repo-Commit = %s", got)
		}
		if resp.ContentLength != int64(len(want)) {
			t.Errorf("Content-Length = %d", resp.ContentLength)
		}
		if resp.Header.Get("Accept-Ranges") != "bytes" {
			t.Errorf("Accept-Ranges = %q", resp.Header.Get("Accept-Ranges"))
		}
	})

	t.Run("GET returns the bytes", func(t *testing.T) {
		for _, rev := range []string{"main", "v1", pushed.SHA} {
			resp := do(t, http.MethodGet, in.url+"/acme/tiny/resolve/"+rev+"/tokenizer/vocab.txt", "", nil, nil)
			if got := body(t, resp); got != modelFiles["tokenizer/vocab.txt"] {
				t.Errorf("at %s got %q", rev, got)
			}
		}
	})

	t.Run("Range resumes a download", func(t *testing.T) {
		resp := do(t, http.MethodGet, fileURL, "", nil, map[string]string{"Range": "bytes=1000-"})
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := body(t, resp); got != want[1000:] {
			t.Fatalf("range body has %d bytes, want %d", len(got), len(want)-1000)
		}
	})

	t.Run("errors carry the codes clients look for", func(t *testing.T) {
		tests := []struct {
			method, path, code string
		}{
			{http.MethodGet, "/api/models/acme/missing/revision/main", "RepoNotFound"},
			{http.MethodGet, "/api/models/acme/tiny/revision/nope", "RevisionNotFound"},
			{http.MethodGet, "/api/models/acme/tiny/tree/nope", "RevisionNotFound"},
			{http.MethodHead, "/acme/tiny/resolve/main/absent.bin", "EntryNotFound"},
			{http.MethodHead, "/acme/tiny/resolve/nope/config.json", "RevisionNotFound"},
			{http.MethodHead, "/acme/missing/resolve/main/config.json", "RepoNotFound"},
			{http.MethodGet, "/acme/tiny/resolve/" + strings.Repeat("0", 40) + "/config.json", "RevisionNotFound"},
		}
		for _, tc := range tests {
			resp := do(t, tc.method, in.url+tc.path, "", nil, nil)
			if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != tc.code {
				t.Errorf("%s %s = %d %q, want 404 %q", tc.method, tc.path, resp.StatusCode, resp.Header.Get("X-Error-Code"), tc.code)
			}
		}
	})

	t.Run("revisions with slashes resolve when encoded", func(t *testing.T) {
		push(t, in, "acme/tiny", map[string]string{"x.txt": "pr"}, client.PushOptions{Branch: "refs/pr/1"})
		resp := do(t, http.MethodGet, in.url+"/api/models/acme/tiny/revision/refs%2Fpr%2F1", "", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("model info status %d", resp.StatusCode)
		}
		resp = do(t, http.MethodGet, in.url+"/acme/tiny/resolve/refs%2Fpr%2F1/x.txt", "", nil, nil)
		if got := body(t, resp); got != "pr" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestAccessControl(t *testing.T) {
	t.Parallel()
	open := start(t, server.Config{AnonymousRead: true})
	private := start(t, server.Config{AnonymousRead: false})
	for _, in := range []*instance{open, private} {
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	}
	blobURL := "/api/v1/blobs/" + sha("new")

	type attempt struct {
		name   string
		in     *instance
		method string
		path   string
		token  func(*instance) string
		body   string
		want   int
	}
	none := func(*instance) string { return "" }
	read := func(i *instance) string { return i.read }
	write := func(i *instance) string { return i.write }
	admin := func(i *instance) string { return i.admin }
	foreign := func(*instance) string { return "hf_a_token_for_some_other_service" }

	tests := []attempt{
		// Reading an open registry needs no token, and a token from another
		// service (a leftover HF_TOKEN) must not get in the way.
		{"open: anonymous download", open, http.MethodGet, "/acme/tiny/resolve/main/config.json", none, "", 200},
		{"open: unknown token still reads", open, http.MethodGet, "/acme/tiny/resolve/main/config.json", foreign, "", 200},
		{"open: anonymous model info", open, http.MethodGet, "/api/models/acme/tiny/revision/main", none, "", 200},
		{"open: anonymous list", open, http.MethodGet, "/api/v1/repos", none, "", 200},

		// A private registry gives nothing away without a token.
		{"private: anonymous download", private, http.MethodGet, "/acme/tiny/resolve/main/config.json", none, "", 401},
		{"private: anonymous model info", private, http.MethodGet, "/api/models/acme/tiny/revision/main", none, "", 401},
		{"private: anonymous tree", private, http.MethodGet, "/api/models/acme/tiny/tree/main", none, "", 401},
		{"private: anonymous list", private, http.MethodGet, "/api/v1/repos", none, "", 401},
		{"private: anonymous stats", private, http.MethodGet, "/api/v1/stats", none, "", 401},
		{"private: unknown token", private, http.MethodGet, "/acme/tiny/resolve/main/config.json", foreign, "", 401},
		{"private: read token downloads", private, http.MethodGet, "/acme/tiny/resolve/main/config.json", read, "", 200},
		{"private: read token lists", private, http.MethodGet, "/api/v1/repos", read, "", 200},

		// Writing always needs a write token, open registry or not.
		{"open: anonymous upload", open, http.MethodPut, blobURL, none, "new", 401},
		{"open: read token upload", open, http.MethodPut, blobURL, read, "new", 403},
		{"open: write token upload", open, http.MethodPut, blobURL, write, "new", 201},
		{"open: anonymous commit", open, http.MethodPost, "/api/v1/repos/acme/x/commits", none, `{}`, 401},
		{"open: read token commit", open, http.MethodPost, "/api/v1/repos/acme/x/commits", read, `{}`, 403},
		{"open: anonymous mirror", open, http.MethodPost, "/api/v1/mirror", none, `{"repo":"a/b"}`, 401},
		{"open: read token mirror", open, http.MethodPost, "/api/v1/mirror", read, `{"repo":"a/b"}`, 403},

		// Destructive and administrative operations need an admin token.
		{"open: anonymous delete", open, http.MethodDelete, "/api/v1/repos/acme/tiny", none, "", 401},
		{"open: write token delete", open, http.MethodDelete, "/api/v1/repos/acme/tiny", write, "", 403},
		{"open: write token lists tokens", open, http.MethodGet, "/api/v1/tokens", write, "", 403},
		{"open: write token creates token", open, http.MethodPost, "/api/v1/tokens", write, `{"name":"x","role":"admin"}`, 403},
		{"open: write token gc", open, http.MethodPost, "/api/v1/gc", write, "", 403},
		{"open: admin lists tokens", open, http.MethodGet, "/api/v1/tokens", admin, "", 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rd io.Reader
			if tc.body != "" {
				rd = strings.NewReader(tc.body)
			}
			resp := do(t, tc.method, tc.in.url+tc.path, tc.token(tc.in), rd, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tc.want, body(t, resp))
			}
		})
	}

	// After all the refused attempts the repository is still there.
	if _, err := open.reg.Resolve(context.Background(), "acme/tiny", "main"); err != nil {
		t.Fatalf("repository damaged by refused requests: %v", err)
	}
}

func TestTokenSecretsAreNeverListed(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	resp := do(t, http.MethodGet, in.url+"/api/v1/tokens", in.admin, nil, nil)
	listing := body(t, resp)
	for _, secret := range []string{in.admin, in.write, in.read} {
		if strings.Contains(listing, secret) {
			t.Fatal("token listing contains a token secret")
		}
	}
}

func TestUploadMustMatchItsDigest(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	wrong := sha("what the client claims")
	resp := do(t, http.MethodPut, in.url+"/api/v1/blobs/"+wrong, in.write, strings.NewReader("what it actually sends"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if _, ok, _ := in.reg.StatBlob(context.Background(), wrong); ok {
		t.Fatal("a blob was recorded under a digest it does not hash to")
	}
	resp = do(t, http.MethodPut, in.url+"/api/v1/blobs/not-a-digest", in.write, strings.NewReader("x"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed digest: status %d, want 400", resp.StatusCode)
	}
}

func TestPushPullRoundTrip(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	ctx := context.Background()
	first := push(t, in, "acme/tiny", modelFiles, client.PushOptions{})

	// A second push that changes one file uploads only that file.
	changed := map[string]string{}
	for k, v := range modelFiles {
		changed[k] = v
	}
	changed["config.json"] = `{"model_type":"tiny","v":2}`
	uploaded := map[string]bool{}
	second, err := client.New(in.url, in.write).Push(ctx, writeTree(t, changed), "acme/tiny", client.PushOptions{
		Branch: "main", Progress: func(p string, _ int64, up bool) { uploaded[p] = up },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !uploaded["config.json"] || uploaded["model.safetensors"] || uploaded["tokenizer/vocab.txt"] {
		t.Fatalf("uploaded = %v, want only config.json", uploaded)
	}

	cl := client.New(in.url, "")
	for _, tc := range []struct {
		rev   string
		files map[string]string
		sha   string
	}{{"main", changed, second.SHA}, {first.SHA, modelFiles, first.SHA}} {
		dir := t.TempDir()
		rev, err := cl.Pull(ctx, "acme/tiny", tc.rev, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rev.SHA != tc.sha {
			t.Errorf("pull %s resolved to %s, want %s", tc.rev, rev.SHA, tc.sha)
		}
		for p, want := range tc.files {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil || string(got) != want {
				t.Errorf("pull %s: %s differs (%v)", tc.rev, p, err)
			}
		}
	}

	t.Run("pull resumes a partial file and repairs a damaged one", func(t *testing.T) {
		dir := t.TempDir()
		want := modelFiles["model.safetensors"]
		// An interrupted download left the first part behind.
		if err := os.WriteFile(filepath.Join(dir, "model.safetensors.part"), []byte(want[:12345]), 0o600); err != nil {
			t.Fatal(err)
		}
		// A finished file was later corrupted on disk.
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("corrupted"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := cl.Pull(ctx, "acme/tiny", first.SHA, dir, nil); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"model.safetensors", "config.json"} {
			got, _ := os.ReadFile(filepath.Join(dir, p))
			if string(got) != modelFiles[p] {
				t.Errorf("%s was not restored", p)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "model.safetensors.part")); !os.IsNotExist(err) {
			t.Error("partial file left behind")
		}
	})
}

func TestDeleteThenStorageIsFreed(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	resp := do(t, http.MethodDelete, in.url+"/api/v1/repos/acme/tiny", in.admin, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	st, err := in.reg.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Repos != 0 || st.Blobs != 0 || st.StoredBytes != 0 {
		t.Fatalf("stats after delete = %+v, want empty", st)
	}
	resp = do(t, http.MethodHead, in.url+"/acme/tiny/resolve/main/config.json", "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted repo still serves files: %d", resp.StatusCode)
	}
}

func TestWebUI(t *testing.T) {
	t.Parallel()
	open := start(t, server.Config{AnonymousRead: true, Version: "test"})
	private := start(t, server.Config{AnonymousRead: false})
	for _, in := range []*instance{open, private} {
		push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	}
	cookie := func(in *instance, token string) map[string]string {
		return map[string]string{"Cookie": "md_token=" + token}
	}

	t.Run("home lists repositories", func(t *testing.T) {
		resp := do(t, http.MethodGet, open.url+"/", "", nil, nil)
		page := body(t, resp)
		if resp.StatusCode != 200 || !strings.Contains(page, "acme/tiny") || !strings.Contains(page, "On disk") {
			t.Fatalf("status %d, page missing repository or storage usage", resp.StatusCode)
		}
	})
	t.Run("repository page shows files and revisions", func(t *testing.T) {
		resp := do(t, http.MethodGet, open.url+"/ui/repo/acme/tiny", "", nil, nil)
		page := body(t, resp)
		for _, want := range []string{"model.safetensors", "tokenizer/vocab.txt", "HF_ENDPOINT=" + open.url, "/acme/tiny/resolve/"} {
			if !strings.Contains(page, want) {
				t.Errorf("page missing %q", want)
			}
		}
		if strings.Contains(page, "Delete repository") {
			t.Error("anonymous visitor is offered the delete form")
		}
	})
	t.Run("hub-style repository link redirects to the page", func(t *testing.T) {
		resp := do(t, http.MethodGet, open.url+"/acme/tiny", "", nil, nil)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/repo/acme/tiny" {
			t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
		}
	})
	t.Run("unknown repository is a 404 page", func(t *testing.T) {
		resp := do(t, http.MethodGet, open.url+"/ui/repo/acme/nope", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("private registry sends visitors to sign in", func(t *testing.T) {
		for _, p := range []string{"/", "/ui/repo/acme/tiny"} {
			resp := do(t, http.MethodGet, private.url+p, "", nil, nil)
			if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/login" {
				t.Errorf("%s: status %d location %q", p, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		resp := do(t, http.MethodGet, private.url+"/", "", nil, cookie(private, private.read))
		if resp.StatusCode != 200 {
			t.Fatalf("signed-in visitor got %d", resp.StatusCode)
		}
	})
	t.Run("sign in rejects bad tokens and sets a locked-down cookie for good ones", func(t *testing.T) {
		form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		resp := do(t, http.MethodPost, open.url+"/ui/login", "", strings.NewReader("token=md_wrong"), form)
		if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
			t.Fatalf("bad token: status %d, %d cookies", resp.StatusCode, len(resp.Cookies()))
		}
		resp = do(t, http.MethodPost, open.url+"/ui/login", "", strings.NewReader("token="+open.admin), form)
		if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 {
			t.Fatalf("good token: status %d, %d cookies", resp.StatusCode, len(resp.Cookies()))
		}
		c := resp.Cookies()[0]
		if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
			t.Fatalf("cookie = %+v, want HttpOnly and SameSite=Strict", c)
		}
	})
	t.Run("delete needs an admin, the typed name, and a same-origin request", func(t *testing.T) {
		form := func(extra map[string]string) map[string]string {
			h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
			for k, v := range extra {
				h[k] = v
			}
			return h
		}
		del := open.url + "/ui/repo/acme/tiny/delete"
		exists := func() bool {
			_, err := open.reg.GetRepo(context.Background(), "acme/tiny")
			return err == nil
		}

		resp := do(t, http.MethodPost, del, "", strings.NewReader("confirm=acme/tiny"), form(nil))
		if resp.StatusCode != http.StatusForbidden || !exists() {
			t.Fatalf("anonymous delete: status %d, repo exists %v", resp.StatusCode, exists())
		}
		resp = do(t, http.MethodPost, del, "", strings.NewReader("confirm=acme/tiny"), form(cookie(open, open.write)))
		if resp.StatusCode != http.StatusForbidden || !exists() {
			t.Fatalf("write-token delete: status %d, repo exists %v", resp.StatusCode, exists())
		}
		// A page on another site submitting the form with the admin's cookie.
		cross := form(cookie(open, open.admin))
		cross["Origin"] = "https://evil.example"
		cross["Sec-Fetch-Site"] = "cross-site"
		resp = do(t, http.MethodPost, del, "", strings.NewReader("confirm=acme/tiny"), cross)
		if resp.StatusCode != http.StatusForbidden || !exists() {
			t.Fatalf("cross-site delete: status %d, repo exists %v", resp.StatusCode, exists())
		}
		resp = do(t, http.MethodPost, del, "", strings.NewReader("confirm=wrong"), form(cookie(open, open.admin)))
		if resp.StatusCode != http.StatusSeeOther || !exists() {
			t.Fatalf("wrong confirmation: status %d, repo exists %v", resp.StatusCode, exists())
		}
		resp = do(t, http.MethodPost, del, "", strings.NewReader("confirm=acme%2Ftiny"), form(cookie(open, open.admin)))
		if resp.StatusCode != http.StatusSeeOther || exists() {
			t.Fatalf("confirmed delete: status %d, repo exists %v", resp.StatusCode, exists())
		}
	})
}

func TestStaticAssetsAndHealth(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: false, Version: "1.2.3"})
	resp := do(t, http.MethodGet, in.url+"/static/app.css", "", nil, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp = do(t, http.MethodGet, in.url+"/healthz", "", nil, nil)
	if got := body(t, resp); !bytes.Contains([]byte(got), []byte(`"1.2.3"`)) {
		t.Fatalf("healthz = %s", got)
	}
}
