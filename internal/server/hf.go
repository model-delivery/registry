// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha1" //nolint:gosec // placeholder ids for directories, not a security boundary.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/registry"
)

// The routes in this file reproduce the part of the Hugging Face Hub HTTP
// API that huggingface_hub uses to download a model or a dataset:
//
//	GET  /api/models/{repo}[/revision/{rev}]   which revision, which files
//	GET  /api/models/{repo}/tree/{rev}[/{dir}] file listing with digests
//	POST /api/models/{repo}/paths-info/{rev}   the same, for named paths
//	GET  /api/models/{repo}/refs               branches and tags
//	HEAD /{repo}/resolve/{rev}/{file}          ETag, size, revision id
//	GET  /{repo}/resolve/{rev}/{file}          the bytes, with Range support
//
// A dataset has the same routes with /api/datasets/ in place of
// /api/models/, and /datasets/{repo}/resolve/ for its files: the Hub's own
// addresses. One handler serves both types; the type comes from the path.
// docs/compatibility.md records what is and is not covered.

// hfModelsRoot and hfDatasetsRoot are where repositories of each type sit
// in the Hub's API. A dataset's other addresses begin with
// kit.DatasetsSegment.
const (
	hfModelsRoot   = "/api/models/"
	hfDatasetsRoot = "/api/datasets/"
)

// hfAPIRoot is the root of the Hub's API for repositories of type typ.
func hfAPIRoot(typ registry.RepoType) string {
	if typ == registry.TypeDataset {
		return hfDatasetsRoot
	}
	return hfModelsRoot
}

// hfAPIPath reads a path under /api/models/ or /api/datasets/ into the
// repository type and the decoded segments after the root.
func hfAPIPath(escaped string) (registry.RepoType, []string, bool) {
	for _, typ := range []registry.RepoType{registry.TypeModel, registry.TypeDataset} {
		if rest, under := strings.CutPrefix(escaped, hfAPIRoot(typ)); under {
			segs, ok := splitPath(rest)
			return typ, segs, ok
		}
	}
	return "", nil, false
}

// mirrorCommand is the command that fetches the repository with key key
// ahead of time, for messages that tell someone to run it.
func mirrorCommand(key string) string {
	if typ, name := registry.SplitRepoKey(key); typ == registry.TypeDataset {
		return "modelctl mirror --repo-type dataset " + name
	}
	return "modelctl mirror " + key
}

const hfTime = "2006-01-02T15:04:05.000Z"

// presignTTL is how long a redirected download URL stays valid. It only
// has to cover starting the transfer; a transfer in progress is not cut off.
const presignTTL = time.Hour

// The names the Hub gives things, which clients send and expect back.
const (
	hfBranchPrefix   = "refs/heads/"
	hfTagPrefix      = "refs/tags/"
	hfEntryFile      = "file"
	hfEntryDirectory = "directory"
	// xetExplanation is where a client asking for a Xet token is sent. Its
	// error shows only the address that failed, never the response body, so
	// the explanation is the address.
	xetExplanation = "/api/xet-is-not-supported/set-HF_HUB_DISABLE_XET=1-and-upload-again"
)

// hfRepos routes a request under /api/models/ or /api/datasets/: the
// repository's type comes from the path, and every route below serves both
// types. It checks nothing itself: every route goes through the gate with
// the role it needs, the upload steps with write and the rest with read.
//
// Anything the Hub's dataset viewer serves under /api/datasets/ (parquet,
// croissant) is not implemented and answers 404, which is how the
// datasets library learns to read the repository's own files instead.
func (s *Server) hfRepos(w http.ResponseWriter, r *http.Request) {
	route := func(minimum registry.Role, h http.HandlerFunc) { s.gate(minimum, asHF, h).ServeHTTP(w, r) }
	answer := func(status int, msg string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { hfError(w, status, msg) }
	}

	typ, segs, ok := hfAPIPath(r.URL.EscapedPath())
	if !ok || len(segs) < 2 {
		// A path that is not {namespace}/{name}/...: the listing and search
		// at /api/models itself, an id of one segment, as old models on the
		// Hub have, or a path with an empty or badly encoded segment.
		route(registry.RoleRead, answer(http.StatusNotFound, "not implemented: use namespace/name repository ids"))
		return
	}
	repo, rest := registry.RepoKey(typ, segs[0]+"/"+segs[1]), segs[2:]
	// is reports whether the path after the repository is {step}/{one segment}.
	is := func(step string) bool { return len(rest) == 2 && rest[0] == step }

	if r.Method == http.MethodPost {
		// The upload steps (hfupload.go). Every POST here needs the write
		// role, including one to a step that does not exist, except
		// paths-info, which only reads and is a POST because it carries a
		// list of paths.
		switch {
		case is("paths-info"):
			route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) { s.hfPathsInfo(w, r, repo, rest[1]) })
		case is("preupload"):
			route(registry.RoleWrite, func(w http.ResponseWriter, r *http.Request) { s.hfPreupload(w, r, repo, rest[1]) })
		case is("commit"):
			route(registry.RoleWrite, func(w http.ResponseWriter, r *http.Request) { s.hfCommit(w, r, repo, rest[1]) })
		case is("branch"):
			route(registry.RoleWrite, func(w http.ResponseWriter, r *http.Request) { s.hfCreateBranch(w, r, repo, rest[1]) })
		default:
			route(registry.RoleWrite, answer(http.StatusNotFound,
				"this Hugging Face endpoint is not implemented by the registry; see docs/compatibility.md"))
		}
		return
	}
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		route(registry.RoleRead, answer(http.StatusMethodNotAllowed,
			"method not allowed; see docs/compatibility.md for what the registry implements"))
	case is("xet-write-token"), is("xet-read-token"):
		// A client with hf_xet installed asks for this before uploading.
		route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, xetExplanation, http.StatusTemporaryRedirect)
		})
	case len(rest) == 0:
		route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) { s.hfRepoInfo(w, r, repo, defaultBranch) })
	case is("revision"):
		route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) { s.hfRepoInfo(w, r, repo, rest[1]) })
	case len(rest) >= 2 && rest[0] == "tree":
		route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) {
			s.hfTree(w, r, repo, rest[1], strings.Join(rest[2:], "/"))
		})
	case len(rest) == 1 && rest[0] == "refs":
		route(registry.RoleRead, func(w http.ResponseWriter, r *http.Request) { s.hfRefs(w, r, repo) })
	default:
		route(registry.RoleRead, answer(http.StatusNotFound, "this Hugging Face endpoint is not implemented by the registry"))
	}
}

// hfRepoInfo answers model_info, dataset_info and repo_info. repo is the
// repository's key.
func (s *Server) hfRepoInfo(w http.ResponseWriter, r *http.Request, repo, rev string) {
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	// Start from what the upstream hub said about a mirrored revision (tags,
	// card data, library name) so tools that read it keep working, then
	// overwrite everything this registry is the authority for.
	doc := map[string]any{}
	if c.UpstreamInfo != "" {
		_ = json.Unmarshal([]byte(c.UpstreamInfo), &doc)
	}
	// With blobs=true (files_metadata=True in huggingface_hub) the Hub also
	// gives each file's size, git blob id and, for a large file, its LFS
	// digest. Without it, the name alone.
	withBlobs := queryTrue(r, "blobs")
	siblings := make([]hfSibling, len(files))
	for i, f := range files {
		siblings[i] = hfSibling{RFilename: f.Path}
		if withBlobs {
			size := f.Size
			siblings[i].Size, siblings[i].BlobID = &size, f.GitOID
			if f.LFS {
				siblings[i].LFS = &hfSiblingLFS{SHA256: f.SHA256, Size: f.Size, PointerSize: lfsPointerSize(f)}
			}
		}
	}
	// The Hub names a repository by its id alone, whatever its type; only a
	// model's description also carries modelId.
	typ, name := registry.SplitRepoKey(repo)
	doc["id"] = name
	if typ.IsZero() {
		doc["modelId"] = name
	}
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

// hfSibling is a file in a repository description.
type hfSibling struct {
	RFilename string        `json:"rfilename"`
	BlobID    string        `json:"blobId,omitempty"`
	Size      *int64        `json:"size,omitempty"`
	LFS       *hfSiblingLFS `json:"lfs,omitempty"`
}

// hfSiblingLFS is how a repository description gives a large file's
// digest; a tree entry gives it as hfLFS.
type hfSiblingLFS struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	PointerSize int    `json:"pointerSize"`
}

// queryTrue reports whether a query parameter is set to true, as the
// Python clients spell it.
func queryTrue(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "true", "True", "1":
		return true
	}
	return false
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
		s.hfFail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	deep := queryTrue(r, "recursive")
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
				out = append(out, dirEntry(c, d))
			}
			if !deep {
				break
			}
		}
		if !deep && strings.Contains(rel, "/") {
			continue
		}
		out = append(out, fileEntry(f))
	}
	if dir != "" && len(out) == 0 {
		s.hfFail(w, r, fmt.Errorf("%w: %s", registry.ErrEntryNotFound, dir))
		return
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// fileEntry is how the Hub lists a file in a tree or paths-info answer.
func fileEntry(f registry.File) hfTreeEntry {
	e := hfTreeEntry{Type: hfEntryFile, OID: f.GitOID, Size: f.Size, Path: f.Path}
	if f.LFS {
		e.LFS = &hfLFS{OID: f.SHA256, Size: f.Size, PointerSize: lfsPointerSize(f)}
	}
	return e
}

// dirEntry is how the Hub lists a directory of revision c. The registry
// keeps no tree objects, so its id is a stable stand-in.
func dirEntry(c registry.Commit, d string) hfTreeEntry {
	sum := sha1.Sum([]byte(c.SHA + ":" + d)) //nolint:gosec // see import note.
	return hfTreeEntry{Type: hfEntryDirectory, OID: hex.EncodeToString(sum[:]), Path: d}
}

// hfPathsInfo answers get_paths_info, which HfFileSystem also calls: the
// tree entries of the paths named in the form field "paths", in the order
// named. A path that is neither a file nor a directory of the revision is
// left out, as the Hub does. "expand" (last commit, security scan) is not
// supported and ignored; the entries are those of a plain listing.
func (s *Server) hfPathsInfo(w http.ResponseWriter, r *http.Request, repo, rev string) {
	paths, err := s.formValues(w, r, "paths")
	if err != nil {
		hfError(w, http.StatusBadRequest, "could not read the form: "+err.Error())
		return
	}
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	out := []hfTreeEntry{}
	for _, p := range paths {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		i, found := sort.Find(len(files), func(i int) int { return strings.Compare(p, files[i].Path) })
		switch {
		case found:
			out = append(out, fileEntry(files[i]))
		case slices.ContainsFunc(files, func(f registry.File) bool { return strings.HasPrefix(f.Path, p+"/") }):
			out = append(out, dirEntry(c, p))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// lfsPointerSize is the length of the git-lfs pointer file for f.
func lfsPointerSize(f registry.File) int {
	return len(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", f.SHA256, f.Size))
}

func (s *Server) hfRefs(w http.ResponseWriter, r *http.Request, repoName string) {
	repo, err := s.reg.GetRepo(r.Context(), repoName)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	refs, err := s.reg.Refs(r.Context(), repo)
	if err != nil {
		s.hfFail(w, r, err)
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
			out["tags"] = append(out["tags"], ref{x.Name, hfTagPrefix + x.Name, x.SHA})
		} else {
			out["branches"] = append(out["branches"], ref{x.Name, hfBranchPrefix + x.Name, x.SHA})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// hfResolve serves a file: filePath at revision rev of the repository with
// key repo.
func (s *Server) hfResolve(w http.ResponseWriter, r *http.Request, repo, rev, filePath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hfError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c, err := s.resolve(r.Context(), repo, rev)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	// From here on the revision is known; clients use this header to pin it.
	w.Header().Set("X-Repo-Commit", c.SHA)
	f, err := s.reg.File(r.Context(), c, filePath)
	if err != nil {
		s.hfFail(w, r, err)
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

	// A download is a GET. It is a cache hit when the content is stored as
	// the request arrives, and a miss when it has to be fetched first.
	if r.Method == http.MethodGet && !f.Pending {
		s.m.downloadHit.Inc()
	}
	if f.Pending {
		// The revision lists this file but its content has not been fetched.
		if r.Method == http.MethodHead {
			// Everything a HEAD reports (identity, size) is in the listing,
			// so a client checking what it would download costs nothing.
			h.Set("Content-Type", "application/octet-stream")
			h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
			h.Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			return
		}
		if s.puller == nil {
			writeHFError(w, http.StatusNotFound, hfCodeEntryNotFound, fmt.Sprintf(
				"%s is listed in this revision but was never fetched from upstream, and pull-through is off: run `%s` or turn pull-through on", filePath, mirrorCommand(repo)), nil)
			return
		}
		// The file is fetched, by this request or by the fetch already in
		// flight for it. Where the rules allow (stream.go), it is sent while
		// it arrives; otherwise, and whenever the fetch ends before a byte
		// could be sent, the request waits for the fetch and is answered
		// from its result.
		s.m.downloadMiss.Inc()
		var err error
		if start, streamed := s.streamFrom(r, f); streamed {
			var job *pullJob[struct{}]
			if job, err = s.puller.fetchFile(r.Context(), c, f); err == nil {
				if s.serveWhileFetching(w, r, job, c, f, start) {
					return
				}
				_, err = job.wait(r.Context())
			}
		} else {
			err = s.puller.ensureFile(r.Context(), c, f)
		}
		if err != nil {
			switch {
			case errors.As(err, new(errFileTooLarge)):
				hfError(w, http.StatusForbidden, err.Error())
			case errors.Is(err, errNotFromUpstream):
				writeHFError(w, http.StatusNotFound, hfCodeEntryNotFound, fmt.Sprintf(
					"%s is listed in this revision but was never fetched, and %s was created in this registry, so pull-through does not fetch into it: run `%s`", filePath, repo, mirrorCommand(repo)), nil)
			default:
				// The upstream's failure or the registry's own: pullFailure
				// says which, and only the first is described to the client.
				s.hfFail(w, r, pullFailure(err))
			}
			return
		}
		if f, err = s.reg.File(r.Context(), c, filePath); err != nil {
			s.hfFail(w, r, err)
			return
		}
	}

	// A request for part of a file is answered with all of it when bytes of
	// that file once left the registry unverified (puller.distrust): what
	// the client wants to add to may be those bytes. A 200 makes it start
	// again. The object store would honour the range, so no redirect either.
	whole := false
	if s.puller != nil && r.Header.Get("Range") != "" && s.puller.distrusts(f.ETag()) {
		r.Header.Del("Range")
		r.Header.Del("If-Range")
		whole = true
	}

	if s.cfg.RedirectDownloads && !whole {
		// Send the client to the object store for the bytes. The headers
		// below are how the Hugging Face Hub describes a file that lives
		// behind a redirect, so clients learn its identity and size from
		// this response and fetch the content from Location.
		location, ok, err := s.reg.PresignBlob(r.Context(), f.SHA256, presignTTL)
		if err != nil {
			s.hfFail(w, r, err)
			return
		}
		if ok {
			h.Set("X-Linked-Etag", `"`+f.ETag()+`"`)
			h.Set("X-Linked-Size", strconv.FormatInt(f.Size, 10))
			h.Set("Location", location)
			h.Set("Cache-Control", "no-store") // the signed URL expires
			if r.Method == http.MethodGet {
				s.m.downloadRedirects.Inc()
			}
			w.WriteHeader(http.StatusFound)
			return
		}
	}

	content, size, err := s.reg.OpenBlob(r.Context(), f.SHA256)
	if err != nil {
		s.log.Error("stored file is missing its blob", "repo", repo, "path", filePath, "sha256", f.SHA256, "error", err)
		hfError(w, http.StatusInternalServerError, "the stored content for this file is missing; the registry's administrator can find every such file with `modelctl verify`")
		return
	}
	defer func() { _ = content.Close() }()
	if size != f.Size {
		s.log.Error("stored blob has the wrong size", "repo", repo, "path", filePath, "sha256", f.SHA256, "want", f.Size, "got", size)
		hfError(w, http.StatusInternalServerError, "the stored content for this file is damaged; the registry's administrator can find every such file with `modelctl verify`")
		return
	}

	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", path.Base(filePath)))
	h.Set("X-Content-Type-Options", "nosniff")
	// ServeContent handles HEAD, Range, If-Range and If-None-Match, and
	// streams straight from the file. The bytes it sends are counted.
	http.ServeContent(&countedWriter{ResponseWriter: w, sent: s.m.bytesFromStore}, r, "", time.Time{}, content)
}

func (s *Server) hfWhoami(w http.ResponseWriter, r *http.Request) {
	tok := s.identity(r)
	if tok.Name == "" {
		hfError(w, http.StatusUnauthorized, "invalid or missing access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "user", "name": tok.Name, "fullname": tok.Name, "orgs": []string{},
		"auth": map[string]any{"type": "access_token", "accessToken": map[string]string{"displayName": tok.Name, "role": string(tok.Role)}},
	})
}
