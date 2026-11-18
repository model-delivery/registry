// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha1" //nolint:gosec // placeholder ids for directories, not a security boundary.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/registry"
)

// The routes in this file reproduce the part of the Hugging Face Hub HTTP
// API that huggingface_hub uses to download a model:
//
//	GET  /api/models/{repo}[/revision/{rev}]   which revision, which files
//	GET  /api/models/{repo}/tree/{rev}[/{dir}] file listing with digests
//	GET  /api/models/{repo}/refs               branches and tags
//	HEAD /{repo}/resolve/{rev}/{file}          ETag, size, revision id
//	GET  /{repo}/resolve/{rev}/{file}          the bytes, with Range support
//
// docs/compatibility.md records what is and is not covered.

const hfTime = "2006-01-02T15:04:05.000Z"

// presignTTL is how long a redirected download URL stays valid. It only
// has to cover starting the transfer; a transfer in progress is not cut off.
const presignTTL = time.Hour

func (s *Server) hfModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "the Hugging Face compatible API is read-only; see docs/compatibility.md")
		return
	}
	segs, ok := splitPath(strings.TrimPrefix(r.URL.EscapedPath(), "/api/models"))
	if !ok || len(segs) < 2 {
		// Listing/search (GET /api/models) and single-segment legacy ids.
		writeError(w, http.StatusNotFound, "not implemented: use namespace/name repository ids")
		return
	}
	repo := segs[0] + "/" + segs[1]
	rest := segs[2:]
	switch {
	case len(rest) == 0:
		s.hfModelInfo(w, r, repo, "main")
	case len(rest) == 2 && rest[0] == "revision":
		s.hfModelInfo(w, r, repo, rest[1])
	case len(rest) >= 2 && rest[0] == "tree":
		s.hfTree(w, r, repo, rest[1], strings.Join(rest[2:], "/"))
	case len(rest) == 1 && rest[0] == "refs":
		s.hfRefs(w, r, repo)
	default:
		writeError(w, http.StatusNotFound, "this Hugging Face endpoint is not implemented by the registry")
	}
}

func (s *Server) hfModelInfo(w http.ResponseWriter, r *http.Request, repo, rev string) {
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Start from what the upstream hub said about a mirrored revision (tags,
	// card data, library name) so tools that read it keep working, then
	// overwrite everything this registry is the authority for.
	doc := map[string]any{}
	if c.UpstreamInfo != "" {
		_ = json.Unmarshal([]byte(c.UpstreamInfo), &doc)
	}
	siblings := make([]map[string]string, len(files))
	for i, f := range files {
		siblings[i] = map[string]string{"rfilename": f.Path}
	}
	doc["id"] = repo
	doc["modelId"] = repo
	doc["sha"] = c.SHA
	doc["siblings"] = siblings
	doc["private"] = false
	doc["gated"] = false
	doc["disabled"] = false
	if _, ok := doc["lastModified"]; !ok {
		doc["lastModified"] = c.CreatedAt.UTC().Format(hfTime)
	}
	if _, ok := doc["tags"]; !ok {
		doc["tags"] = []string{}
	}
	writeJSON(w, http.StatusOK, doc)
}

type hfLFS struct {
	OID         string `json:"oid"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize"`
}

type hfTreeEntry struct {
	Type string `json:"type"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *hfLFS `json:"lfs,omitempty"`
}

func (s *Server) hfTree(w http.ResponseWriter, r *http.Request, repo, rev, dir string) {
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	recursive := r.URL.Query().Get("recursive")
	deep := recursive == "true" || recursive == "True" || recursive == "1"
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}

	out := []hfTreeEntry{}
	dirs := map[string]bool{}
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rel := f.Path[len(prefix):]
		// Every directory between the listed one and the file is an entry.
		for i, ch := range rel {
			if ch != '/' {
				continue
			}
			d := prefix + rel[:i]
			if !dirs[d] {
				dirs[d] = true
				sum := sha1.Sum([]byte(c.SHA + ":" + d)) //nolint:gosec // see import note.
				out = append(out, hfTreeEntry{Type: "directory", OID: hex.EncodeToString(sum[:]), Path: d})
			}
			if !deep {
				break
			}
		}
		if !deep && strings.Contains(rel, "/") {
			continue
		}
		e := hfTreeEntry{Type: "file", OID: f.GitOID, Size: f.Size, Path: f.Path}
		if f.LFS {
			e.LFS = &hfLFS{OID: f.SHA256, Size: f.Size, PointerSize: lfsPointerSize(f)}
		}
		out = append(out, e)
	}
	if dir != "" && len(out) == 0 {
		s.fail(w, r, fmt.Errorf("%w: %s", registry.ErrEntryNotFound, dir))
		return
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// lfsPointerSize is the length of the git-lfs pointer file for f.
func lfsPointerSize(f registry.File) int {
	return len(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", f.SHA256, f.Size))
}

func (s *Server) hfRefs(w http.ResponseWriter, r *http.Request, repoName string) {
	repo, err := s.reg.GetRepo(r.Context(), repoName)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	refs, err := s.reg.Refs(r.Context(), repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type ref struct {
		Name         string `json:"name"`
		Ref          string `json:"ref"`
		TargetCommit string `json:"targetCommit"`
	}
	out := map[string][]ref{"branches": {}, "tags": {}, "converts": {}}
	for _, x := range refs {
		if x.Kind == registry.KindTag {
			out["tags"] = append(out["tags"], ref{x.Name, "refs/tags/" + x.Name, x.SHA})
		} else {
			out["branches"] = append(out["branches"], ref{x.Name, "refs/heads/" + x.Name, x.SHA})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hfResolve serves a file. segs is {ns, name, "resolve", rev, path...}.
func (s *Server) hfResolve(w http.ResponseWriter, r *http.Request, segs []string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	repo, rev, filePath := segs[0]+"/"+segs[1], segs[3], strings.Join(segs[4:], "/")
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// From here on the revision is known; clients use this header to pin it.
	w.Header().Set("X-Repo-Commit", c.SHA)
	f, err := s.reg.File(r.Context(), c, filePath)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("ETag", `"`+f.ETag()+`"`)
	if registry.IsSHA(rev) {
		// A revision id never changes meaning, so its files can be cached forever.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}

	if s.cfg.RedirectDownloads {
		// Send the client to the object store for the bytes. The headers
		// below are how the Hugging Face Hub describes a file that lives
		// behind a redirect, so clients learn its identity and size from
		// this response and fetch the content from Location.
		location, ok, err := s.reg.PresignBlob(r.Context(), f.SHA256, presignTTL)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if ok {
			h.Set("X-Linked-Etag", `"`+f.ETag()+`"`)
			h.Set("X-Linked-Size", strconv.FormatInt(f.Size, 10))
			h.Set("Location", location)
			h.Set("Cache-Control", "no-store") // the signed URL expires
			w.WriteHeader(http.StatusFound)
			return
		}
	}

	content, size, err := s.reg.OpenBlob(r.Context(), f.SHA256)
	if err != nil {
		s.log.Error("stored file is missing its blob", "repo", repo, "path", filePath, "sha256", f.SHA256, "error", err)
		writeError(w, http.StatusInternalServerError, "the stored content for this file is missing; run a registry check")
		return
	}
	defer func() { _ = content.Close() }()
	if size != f.Size {
		s.log.Error("stored blob has the wrong size", "repo", repo, "path", filePath, "sha256", f.SHA256, "want", f.Size, "got", size)
		writeError(w, http.StatusInternalServerError, "the stored content for this file is damaged")
		return
	}

	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", path.Base(filePath)))
	h.Set("X-Content-Type-Options", "nosniff")
	// ServeContent handles HEAD, Range, If-Range and If-None-Match, and
	// streams straight from the file.
	http.ServeContent(w, r, "", time.Time{}, content)
}

func (s *Server) hfWhoami(w http.ResponseWriter, r *http.Request) {
	_, tok := s.roleOf(r)
	if tok.Name == "" {
		writeError(w, http.StatusUnauthorized, "invalid or missing access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "user", "name": tok.Name, "fullname": tok.Name, "orgs": []string{},
		"auth": map[string]any{"type": "access_token", "accessToken": map[string]string{"displayName": tok.Name, "role": tok.Role}},
	})
}
