// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// These tests speak the Hugging Face upload protocol by hand, to reach the
// edges the real client never exercises. test/e2e runs the real client.

func jsonBody(t *testing.T, v any) *strings.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(b))
}

// commitBody builds the line-delimited body of a commit request.
func commitBody(t *testing.T, lines ...map[string]any) *strings.Reader {
	t.Helper()
	var sb strings.Builder
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return strings.NewReader(sb.String())
}

func header(summary, parent string) map[string]any {
	v := map[string]any{"summary": summary, "description": ""}
	if parent != "" {
		v["parentCommit"] = parent
	}
	return map[string]any{"key": "header", "value": v}
}

func inlineFile(path, content string) map[string]any {
	return map[string]any{"key": "file", "value": map[string]any{
		"path": path, "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)),
	}}
}

func lfsFile(path, content string) map[string]any {
	return map[string]any{"key": "lfsFile", "value": map[string]any{
		"path": path, "algo": "sha256", "oid": sha(content), "size": len(content),
	}}
}

// batch asks where to upload objects and returns the upload URL per digest
// ("" when the registry says it already has the object).
func batch(t *testing.T, in *instance, repo string, contents ...string) map[string]string {
	t.Helper()
	objects := make([]map[string]any, len(contents))
	for i, c := range contents {
		objects[i] = map[string]any{"oid": sha(c), "size": len(c)}
	}
	resp := do(t, http.MethodPost, in.url+"/"+repo+".git/info/lfs/objects/batch", in.write,
		jsonBody(t, map[string]any{"operation": "upload", "transfers": []string{"basic", "multipart"}, "objects": objects, "hash_algo": "sha256"}), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: status %d (%s)", resp.StatusCode, body(t, resp))
	}
	var out struct {
		Transfer string `json:"transfer"`
		Objects  []struct {
			OID     string `json:"oid"`
			Actions map[string]struct {
				Href string `json:"href"`
			} `json:"actions"`
		} `json:"objects"`
	}
	decode(t, resp, &out)
	if out.Transfer != "basic" {
		t.Fatalf("transfer = %q", out.Transfer)
	}
	urls := map[string]string{}
	for _, o := range out.Objects {
		urls[o.OID] = o.Actions["upload"].Href
	}
	return urls
}

func commitTo(t *testing.T, in *instance, repo, branch, token string, lines ...map[string]any) (int, string) {
	t.Helper()
	resp := do(t, http.MethodPost, in.url+"/api/models/"+repo+"/commit/"+url.PathEscape(branch), token, commitBody(t, lines...),
		map[string]string{"Content-Type": "application/x-ndjson"})
	return resp.StatusCode, body(t, resp)
}

func commitOID(t *testing.T, responseBody string) string {
	t.Helper()
	var out struct {
		CommitOID string `json:"commitOid"`
	}
	if err := json.Unmarshal([]byte(responseBody), &out); err != nil || !registry.IsSHA(out.CommitOID) {
		t.Fatalf("commit response %q has no revision id", responseBody)
	}
	return out.CommitOID
}

func TestLargeFileUploadURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := start(t, server.Config{AnonymousRead: true})
	content := strings.Repeat("large file content ", 4096)
	href := batch(t, in, "acme/up", content)[sha(content)]
	if href == "" {
		t.Fatal("no upload URL for an object the registry does not have")
	}
	stored := func(c string) bool {
		_, ok, _ := in.reg.StatBlob(ctx, sha(c))
		return ok
	}

	t.Run("the URL only accepts the bytes it was issued for", func(t *testing.T) {
		// The client sends no token with this request: the URL is the grant.
		resp := do(t, http.MethodPut, href, "", strings.NewReader("something else entirely"), nil)
		if resp.StatusCode != http.StatusBadRequest || stored(content) {
			t.Fatalf("wrong bytes: status %d, stored %v", resp.StatusCode, stored(content))
		}
		short := content[:len(content)-1]
		resp = do(t, http.MethodPut, href, "", strings.NewReader(short), nil)
		if resp.StatusCode != http.StatusBadRequest || stored(content) || stored(short) {
			t.Fatalf("truncated upload: status %d", resp.StatusCode)
		}
	})

	t.Run("a changed URL is refused", func(t *testing.T) {
		u, err := url.Parse(href)
		if err != nil {
			t.Fatal(err)
		}
		other := "a different object"
		tamper := map[string]func(q url.Values, path *string){
			"another digest": func(_ url.Values, path *string) {
				*path = strings.Replace(*path, sha(content), sha(other), 1)
			},
			"another size":      func(q url.Values, _ *string) { q.Set("size", strconv.Itoa(len(other))) },
			"a later expiry":    func(q url.Values, _ *string) { q.Set("expires", "99999999999") },
			"another token":     func(q url.Values, _ *string) { q.Set("token", "admin") },
			"no token":          func(q url.Values, _ *string) { q.Del("token") },
			"no signature":      func(q url.Values, _ *string) { q.Del("signature") },
			"a wrong signature": func(q url.Values, _ *string) { q.Set("signature", strings.Repeat("0", 64)) },
		}
		for name, change := range tamper {
			q, path := u.Query(), u.Path
			change(q, &path)
			target := u.Scheme + "://" + u.Host + path + "?" + q.Encode()
			resp := do(t, http.MethodPut, target, "", strings.NewReader(other), nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
			}
			if stored(other) {
				t.Fatalf("%s: the upload was stored", name)
			}
		}
	})

	t.Run("a URL from another server instance is refused", func(t *testing.T) {
		// Signing keys are per process, so a URL cannot be replayed elsewhere.
		elsewhere := start(t, server.Config{AnonymousRead: true})
		target := strings.Replace(href, in.url, elsewhere.url, 1)
		resp := do(t, http.MethodPut, target, "", strings.NewReader(content), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d, want 403", resp.StatusCode)
		}
	})

	t.Run("the right bytes are stored, and then not asked for again", func(t *testing.T) {
		resp := do(t, http.MethodPut, href, "", strings.NewReader(content), nil)
		if resp.StatusCode != http.StatusOK || !stored(content) {
			t.Fatalf("status %d, stored %v", resp.StatusCode, stored(content))
		}
		if again := batch(t, in, "acme/up", content)[sha(content)]; again != "" {
			t.Fatal("the registry asked for an object it already has")
		}
	})

	t.Run("malformed objects are reported per object", func(t *testing.T) {
		resp := do(t, http.MethodPost, in.url+"/acme/up.git/info/lfs/objects/batch", in.write,
			jsonBody(t, map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": "not-a-digest", "size": 3}}}), nil)
		if got := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, `"error"`) || strings.Contains(got, `"actions"`) {
			t.Fatalf("status %d body %s", resp.StatusCode, got)
		}
	})

	t.Run("downloads do not use this endpoint", func(t *testing.T) {
		resp := do(t, http.MethodPost, in.url+"/acme/up.git/info/lfs/objects/batch", in.write,
			jsonBody(t, map[string]any{"operation": "download", "objects": []map[string]any{}}), nil)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}

// An upload URL is issued to a token, and is worth nothing once that token
// is revoked or loses the write role.
func TestUploadURLEndsWithItsToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := strings.Repeat("uploaded after the token was gone ", 512)
	refused := func(t *testing.T, in *instance, href string) {
		t.Helper()
		resp := do(t, http.MethodPut, href, "", strings.NewReader(content), nil)
		msg := body(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(msg, "revoked or no longer has the write role") {
			t.Fatalf("status %d, want 403 saying why: %s", resp.StatusCode, msg)
		}
		if _, ok, _ := in.reg.StatBlob(ctx, sha(content)); ok {
			t.Fatal("the upload was stored")
		}
	}

	t.Run("revoked", func(t *testing.T) {
		t.Parallel()
		in := start(t, server.Config{AnonymousRead: true})
		href := batch(t, in, "acme/up", content)[sha(content)]
		if err := in.reg.RevokeToken(ctx, "write"); err != nil {
			t.Fatal(err)
		}
		refused(t, in, href)
	})
	t.Run("reduced to the read role", func(t *testing.T) {
		t.Parallel()
		in := start(t, server.Config{AnonymousRead: true})
		href := batch(t, in, "acme/up", content)[sha(content)]
		if err := in.reg.SetToken(ctx, "write", registry.RoleRead, in.write, true); err != nil {
			t.Fatal(err)
		}
		refused(t, in, href)
	})
	t.Run("still valid: accepted, with a name that needs escaping", func(t *testing.T) {
		t.Parallel()
		in := start(t, server.Config{AnonymousRead: true})
		secret, err := in.reg.CreateToken(ctx, "ci & deploy/main", registry.RoleWrite)
		if err != nil {
			t.Fatal(err)
		}
		in.write = secret
		href := batch(t, in, "acme/up", content)[sha(content)]
		if resp := do(t, http.MethodPut, href, "", strings.NewReader(content), nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d (%s)", resp.StatusCode, body(t, resp))
		}
	})
}

func TestUploadCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := start(t, server.Config{AnonymousRead: true})
	big := strings.Repeat("big ", 5000)
	if resp := do(t, http.MethodPut, batch(t, in, "acme/up", big)[sha(big)], "", strings.NewReader(big), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: %d", resp.StatusCode)
	}

	status, out := commitTo(t, in, "acme/up", "main", in.write, header("first", ""),
		inlineFile("config.json", "{}"), inlineFile("empty.txt", ""), lfsFile("model.bin", big), inlineFile("dir/a.txt", "a"), inlineFile("dir/b.txt", "b"))
	if status != http.StatusOK {
		t.Fatalf("first commit: %d %s", status, out)
	}
	first := commitOID(t, out)

	files := func(rev string) map[string]registry.File {
		c, err := in.reg.Resolve(ctx, "acme/up", rev)
		if err != nil {
			t.Fatal(err)
		}
		list, _ := in.reg.Files(ctx, c)
		m := map[string]registry.File{}
		for _, f := range list {
			m[f.Path] = f
		}
		return m
	}

	t.Run("inline and large files are identified the way the Hub identifies them", func(t *testing.T) {
		got := files(first)
		if len(got) != 5 {
			t.Fatalf("%d files", len(got))
		}
		if f := got["model.bin"]; !f.LFS || f.ETag() != sha(big) {
			t.Errorf("large file: %+v", f)
		}
		// A file sent inline is a git-style file: its ETag is its git blob id.
		if f := got["config.json"]; f.LFS || len(f.ETag()) != 40 || f.SHA256 != sha("{}") {
			t.Errorf("inline file: %+v", f)
		}
		resp := do(t, http.MethodHead, in.url+"/acme/up/resolve/main/config.json", "", nil, nil)
		if resp.Header.Get("ETag") != `"`+got["config.json"].GitOID+`"` {
			t.Errorf("served ETag %s", resp.Header.Get("ETag"))
		}
	})

	t.Run("a later commit adds, replaces and deletes on top of the branch", func(t *testing.T) {
		status, out := commitTo(t, in, "acme/up", "main", in.write, header("second", first),
			inlineFile("config.json", `{"v":2}`),
			map[string]any{"key": "deletedFile", "value": map[string]any{"path": "empty.txt"}},
			map[string]any{"key": "deletedFolder", "value": map[string]any{"path": "dir/"}},
			inlineFile("new.txt", "new"))
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, out)
		}
		got := files("main")
		var names []string
		for p := range got {
			names = append(names, p)
		}
		if len(got) != 3 || got["config.json"].SHA256 != sha(`{"v":2}`) || got["model.bin"].SHA256 != sha(big) || got["new.txt"].SHA256 != sha("new") {
			t.Fatalf("files after second commit: %v", names)
		}
		// The first revision is untouched.
		if len(files(first)) != 5 {
			t.Fatal("an earlier revision changed")
		}
	})

	head := func() string {
		c, err := in.reg.Resolve(ctx, "acme/up", "main")
		if err != nil {
			t.Fatal(err)
		}
		return c.SHA
	}

	refused := []struct {
		name   string
		branch string
		query  string
		lines  []map[string]any
		want   int
		say    string
	}{
		{"made against a revision the branch has moved past", "main", "", []map[string]any{header("stale", first), inlineFile("x", "x")}, http.StatusPreconditionFailed, "moved"},
		{"names a large file that was never uploaded", "main", "", []map[string]any{header("m", ""), lfsFile("ghost.bin", "never uploaded")}, http.StatusBadRequest, "HF_HUB_DISABLE_XET"},
		{"declares the wrong size for a large file", "main", "", []map[string]any{header("m", ""),
			{"key": "lfsFile", "value": map[string]any{"path": "model.bin", "algo": "sha256", "oid": sha(big), "size": 1}}}, http.StatusBadRequest, "bytes"},
		{"has no header", "main", "", []map[string]any{inlineFile("x", "x")}, http.StatusBadRequest, "header"},
		{"uses an unknown operation", "main", "", []map[string]any{header("m", ""), {"key": "renamedFile", "value": map[string]any{}}}, http.StatusBadRequest, "not supported"},
		{"sends content that is not base64", "main", "", []map[string]any{header("m", ""),
			{"key": "file", "value": map[string]any{"path": "x", "encoding": "base64", "content": "!!!"}}}, http.StatusBadRequest, "base64"},
		{"writes outside the repository", "main", "", []map[string]any{header("m", ""), inlineFile("../escape", "x")}, http.StatusBadRequest, "path"},
		{"targets a revision id", strings.Repeat("a", 40), "", []map[string]any{header("m", ""), inlineFile("x", "x")}, http.StatusBadRequest, "never change"},
		{"asks for a pull request", "main", "?create_pr=1", []map[string]any{header("m", ""), inlineFile("x", "x")}, http.StatusBadRequest, "pull requests"},
		{"would leave the revision empty", "main", "", []map[string]any{header("m", ""),
			{"key": "deletedFile", "value": map[string]any{"path": "config.json"}},
			{"key": "deletedFile", "value": map[string]any{"path": "model.bin"}},
			{"key": "deletedFile", "value": map[string]any{"path": "new.txt"}}}, http.StatusBadRequest, "at least one file"},
	}
	for _, tc := range refused {
		t.Run("refuses a commit that "+tc.name, func(t *testing.T) {
			before := head()
			resp := do(t, http.MethodPost, in.url+"/api/models/acme/up/commit/"+tc.branch+tc.query, in.write, commitBody(t, tc.lines...), nil)
			got := body(t, resp)
			if resp.StatusCode != tc.want || !strings.Contains(got, tc.say) {
				t.Fatalf("status %d, want %d mentioning %q: %s", resp.StatusCode, tc.want, tc.say, got)
			}
			if resp.Header.Get("X-Error-Message") == "" {
				t.Error("no X-Error-Message: the client would show a bare status code")
			}
			if head() != before {
				t.Fatal("a refused commit moved the branch")
			}
		})
	}

	t.Run("refuses an inline file larger than the limit", func(t *testing.T) {
		before := head()
		huge := strings.Repeat("x", 23<<20) // encodes to more than one commit line may hold
		status, out := commitTo(t, in, "acme/up", "main", in.write, header("m", ""), inlineFile("huge.bin", huge))
		if status != http.StatusRequestEntityTooLarge || !strings.Contains(out, "large-file upload") {
			t.Fatalf("status %d: %s", status, out)
		}
		if head() != before {
			t.Fatal("a refused commit moved the branch")
		}
	})
}

func TestUploadRepositoriesAndBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := start(t, server.Config{AnonymousRead: true})

	create := func(token string, payload map[string]any) (int, string) {
		resp := do(t, http.MethodPost, in.url+"/api/repos/create", token, jsonBody(t, payload), nil)
		return resp.StatusCode, body(t, resp)
	}

	status, out := create(in.write, map[string]any{"name": "model", "organization": "acme", "type": "model"})
	if status != http.StatusOK || !strings.Contains(out, in.url+"/acme/model") {
		t.Fatalf("create: %d %s", status, out)
	}
	// exist_ok clients treat this status as success.
	if status, _ := create(in.write, map[string]any{"name": "model", "organization": "acme"}); status != http.StatusConflict {
		t.Fatalf("create again: %d, want 409", status)
	}
	// No namespace given: the token's name stands in for the user's.
	status, out = create(in.write, map[string]any{"name": "mine"})
	if status != http.StatusOK || !strings.Contains(out, "/write/mine") {
		t.Fatalf("create without a namespace: %d %s", status, out)
	}
	for name, payload := range map[string]map[string]any{
		"a dataset":            {"name": "d", "organization": "acme", "type": "dataset"},
		"a space":              {"name": "s", "organization": "acme", "type": "space"},
		"a bad name":           {"name": "has space", "organization": "acme"},
		"a reserved namespace": {"name": "x", "organization": "api"},
	} {
		if status, out := create(in.write, payload); status != http.StatusBadRequest {
			t.Errorf("%s: status %d %s", name, status, out)
		}
	}

	t.Run("an empty repository exists, with no revision", func(t *testing.T) {
		if _, err := in.reg.GetRepo(ctx, "acme/model"); err != nil {
			t.Fatal(err)
		}
		resp := do(t, http.MethodGet, in.url+"/api/models/acme/model/revision/main", "", nil, nil)
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Error-Code") != "RevisionNotFound" {
			t.Fatalf("status %d code %q", resp.StatusCode, resp.Header.Get("X-Error-Code"))
		}
		page := do(t, http.MethodGet, in.url+"/ui/repo/acme/model", "", nil, nil)
		if got := body(t, page); page.StatusCode != http.StatusOK || !strings.Contains(got, "no revisions yet") {
			t.Fatalf("empty repository page: status %d", page.StatusCode)
		}
	})

	branch := func(repo, name string, payload map[string]any) int {
		resp := do(t, http.MethodPost, in.url+"/api/models/"+repo+"/branch/"+url.PathEscape(name), in.write, jsonBody(t, payload), nil)
		return resp.StatusCode
	}

	t.Run("a branch on an empty repository is made by its first commit", func(t *testing.T) {
		if got := branch("acme/model", "dev", map[string]any{}); got != http.StatusOK {
			t.Fatalf("create_branch on an empty repository: %d", got)
		}
		status, out := commitTo(t, in, "acme/model", "dev", in.write, header("on dev", ""), inlineFile("a.txt", "a"))
		if status != http.StatusOK {
			t.Fatalf("%d %s", status, out)
		}
		c, err := in.reg.Resolve(ctx, "acme/model", "dev")
		if err != nil || c.SHA != commitOID(t, out) {
			t.Fatalf("dev = %v, %v", c.SHA, err)
		}
		if _, err := in.reg.Resolve(ctx, "acme/model", "main"); !errors.Is(err, registry.ErrRevisionNotFound) {
			t.Fatalf("main was created too: %v", err)
		}
	})

	t.Run("a branch from an existing revision", func(t *testing.T) {
		if got := branch("acme/model", "release", map[string]any{"startingPoint": "dev"}); got != http.StatusOK {
			t.Fatalf("status %d", got)
		}
		dev, _ := in.reg.Resolve(ctx, "acme/model", "dev")
		rel, err := in.reg.Resolve(ctx, "acme/model", "release")
		if err != nil || rel.SHA != dev.SHA {
			t.Fatalf("release = %v, %v", rel.SHA, err)
		}
		if got := branch("acme/model", "release", map[string]any{"startingPoint": "dev"}); got != http.StatusConflict {
			t.Fatalf("duplicate branch: %d", got)
		}
		if got := branch("acme/absent", "x", map[string]any{}); got != http.StatusNotFound {
			t.Fatalf("branch on a missing repository: %d", got)
		}
	})
}

func TestUploadAccessControl(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	content := "needs a token"
	steps := []struct {
		name, method, path string
		body               func() *strings.Reader
	}{
		{"create a repository", http.MethodPost, "/api/repos/create", func() *strings.Reader {
			return jsonBody(t, map[string]any{"name": "x", "organization": "acme"})
		}},
		{"ask how to upload", http.MethodPost, "/api/models/acme/x/preupload/main", func() *strings.Reader {
			return jsonBody(t, map[string]any{"files": []map[string]any{{"path": "a", "size": 1, "sample": ""}}})
		}},
		{"ask where to upload", http.MethodPost, "/acme/x.git/info/lfs/objects/batch", func() *strings.Reader {
			return jsonBody(t, map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": sha(content), "size": len(content)}}})
		}},
		{"commit", http.MethodPost, "/api/models/acme/x/commit/main", func() *strings.Reader {
			return commitBody(t, header("m", ""), inlineFile("a", "a"))
		}},
		{"create a branch", http.MethodPost, "/api/models/acme/x/branch/dev", func() *strings.Reader { return jsonBody(t, map[string]any{}) }},
	}
	for _, step := range steps {
		for who, want := range map[string]int{"": http.StatusUnauthorized, in.read: http.StatusForbidden, "hf_some_other_services_token": http.StatusUnauthorized} {
			resp := do(t, step.method, in.url+step.path, who, step.body(), nil)
			if resp.StatusCode != want {
				t.Errorf("%s with token %q: status %d, want %d", step.name, firstN(who, 8), resp.StatusCode, want)
			}
		}
	}
	if _, err := in.reg.GetRepo(context.Background(), "acme/x"); err == nil {
		t.Fatal("a caller without write access created a repository")
	}
	// An upload URL cannot be made up: it has to come from the registry.
	forged := fmt.Sprintf("%s/api/lfs/upload/%s?size=%d&expires=99999999999&signature=%s", in.url, sha(content), len(content), strings.Repeat("ab", 32))
	if resp := do(t, http.MethodPut, forged, in.write, strings.NewReader(content), nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a forged upload URL was accepted even with a write token: %d", resp.StatusCode)
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func TestXetIsDeclinedWithTheFixInTheAddress(t *testing.T) {
	t.Parallel()
	in := start(t, server.Config{AnonymousRead: true})
	// The Xet client prints the address that failed and nothing else, so the
	// explanation has to be in the address.
	resp := do(t, http.MethodGet, in.url+"/api/models/acme/x/xet-write-token/main", in.write, nil, nil)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.Contains(loc, "HF_HUB_DISABLE_XET=1") {
		t.Fatalf("status %d location %q", resp.StatusCode, loc)
	}
	final := do(t, http.MethodGet, in.url+loc, in.write, nil, nil)
	if got := body(t, final); final.StatusCode != http.StatusBadRequest || !strings.Contains(got, "HF_HUB_DISABLE_XET=1") {
		t.Fatalf("status %d body %s", final.StatusCode, got)
	}
}
