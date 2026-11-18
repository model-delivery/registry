// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// The registry's own API, documented in docs/api.md.

func repoParam(r *http.Request) string { return r.PathValue("ns") + "/" + r.PathValue("name") }

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) apiListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.reg.ListRepos(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// RepoDetail is the body of GET /api/v1/repos/{repo}.
type RepoDetail struct {
	registry.Repo
	Refs      []registry.Ref           `json:"refs"`
	Revisions []registry.CommitSummary `json:"revisions"`
}

func (s *Server) repoDetail(r *http.Request, name string) (RepoDetail, error) {
	repo, err := s.reg.GetRepo(r.Context(), name)
	if err != nil {
		return RepoDetail{}, err
	}
	refs, err := s.reg.Refs(r.Context(), repo)
	if err != nil {
		return RepoDetail{}, err
	}
	commits, err := s.reg.Commits(r.Context(), repo)
	if err != nil {
		return RepoDetail{}, err
	}
	return RepoDetail{Repo: repo, Refs: refs, Revisions: commits}, nil
}

func (s *Server) apiGetRepo(w http.ResponseWriter, r *http.Request) {
	d, err := s.repoDetail(r, repoParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// RevisionDetail is the body of GET /api/v1/repos/{repo}/revisions/{rev}.
type RevisionDetail struct {
	registry.Commit
	Files []registry.File `json:"files"`
}

func (s *Server) apiGetRevision(w http.ResponseWriter, r *http.Request) {
	c, err := s.reg.Resolve(r.Context(), repoParam(r), r.PathValue("rev"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, RevisionDetail{Commit: c, Files: files})
}

// CommitRequest is the body of POST /api/v1/repos/{repo}/commits.
type CommitRequest struct {
	Branch  string   `json:"branch"`
	Tags    []string `json:"tags"`
	Message string   `json:"message"`
	// Merge keeps the branch's existing files and overlays these on top.
	// The default replaces the file list.
	Merge bool `json:"merge"`
	Files []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func (s *Server) apiCommit(w http.ResponseWriter, r *http.Request) {
	var req CommitRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Branch == "" {
		req.Branch = "main"
	}
	in := registry.CommitInput{Repo: repoParam(r), Branch: req.Branch, Tags: req.Tags, Message: req.Message,
		Source: registry.SourcePush, Merge: req.Merge}
	for _, f := range req.Files {
		// Pushed files are recorded the way the hub records LFS files, so
		// clients identify them by SHA-256.
		in.Files = append(in.Files, registry.File{Path: f.Path, SHA256: f.SHA256, LFS: true})
	}
	c, err := s.reg.Commit(r.Context(), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("revision recorded", "repo", c.Repo, "sha", c.SHA, "branch", req.Branch)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) apiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	res, err := s.reg.DeleteRepo(r.Context(), repoParam(r))
	if err != nil && res.Blobs == 0 {
		s.fail(w, r, err)
		return
	}
	s.log.Info("repository deleted", "repo", repoParam(r), "blobs_freed", res.Blobs, "bytes_freed", res.Bytes)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiStatBlob(w http.ResponseWriter, r *http.Request) {
	// Asking whether a blob exists is what a push does before deciding not
	// to upload it, so it also marks the blob as in use.
	info, ok, err := s.reg.TouchBlob(r.Context(), r.PathValue("sha256"))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(info.Size))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) apiPutBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !blob.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "blob name must be a lowercase hex SHA-256")
		return
	}
	// The body is streamed to disk and hashed as it arrives; the upload is
	// rejected unless it hashes to the digest in the URL.
	info, err := s.reg.PutBlob(r.Context(), r.Body, blob.Expect{SHA256: digest, Size: r.ContentLength})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": info.SHA256, "size": info.Size})
}

// MirrorRequest is the body of POST /api/v1/mirror.
type MirrorRequest struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	// UpstreamToken authenticates to the upstream hub for private or gated
	// repositories. It is used for this request only and never stored.
	UpstreamToken string `json:"upstream_token"`
}

func (s *Server) apiMirror(w http.ResponseWriter, r *http.Request) {
	var req MirrorRequest
	if !decodeBody(w, r, &req) {
		return
	}
	up := &mirror.Upstream{Endpoint: s.cfg.Upstream, Token: s.cfg.UpstreamToken, Client: s.cfg.UpstreamClient}
	if req.UpstreamToken != "" {
		up.Token = req.UpstreamToken
	}
	// Resolve upstream before answering, so a wrong name or a missing
	// token comes back as a proper HTTP error.
	plan, err := mirror.Prepare(r.Context(), up, req.Repo, req.Revision)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// From here the response is a stream of JSON lines; failures are
	// reported as a final {"event":"error"} line.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	emit := func(e mirror.Event) {
		_ = enc.Encode(e)
		_ = rc.Flush()
	}
	started := time.Now()
	// Progress callbacks arrive from several goroutines; serialise writes.
	events := make(chan mirror.Event, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			emit(e)
		}
	}()
	final, err := plan.Execute(r.Context(), s.reg, 4, func(e mirror.Event) { events <- e })
	close(events)
	<-done
	if err != nil {
		s.log.Warn("mirror failed", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
		msg := err.Error()
		if status, _ := statusFor(err); status == http.StatusInternalServerError && !errors.Is(err, blob.ErrMismatch) {
			msg = "mirror failed: " + msg
		}
		emit(mirror.Event{Event: "error", Error: msg})
		return
	}
	s.log.Info("mirror complete", "repo", final.Repo, "sha", final.SHA, "files", final.Files,
		"downloaded_bytes", final.DownloadedBytes, "reused_bytes", final.ReusedBytes, "took", time.Since(started).Round(time.Millisecond))
	emit(final)
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.reg.Stats(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) apiGC(w http.ResponseWriter, r *http.Request) {
	// Unreferenced blobs younger than this may belong to a push that has
	// uploaded its files but not yet recorded its revision.
	res, err := s.reg.GC(r.Context(), time.Hour)
	if err != nil && res.Blobs == 0 {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.reg.ListTokens(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": toks})
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	secret, err := s.reg.CreateToken(r.Context(), req.Name, req.Role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("token created", "name", req.Name, "role", req.Role)
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "role": req.Role, "token": secret})
}

func (s *Server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.RevokeToken(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("token revoked", "name", r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}
