// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// The registry's own API, documented in docs/api.md.

// defaultBranch is the branch meant when a request names none.
const defaultBranch = "main"

// mirrorEventBuffer is how many progress lines of a mirror may wait to be
// written to the client before the file fetches that report them wait too.
const mirrorEventBuffer = 64

func repoParam(r *http.Request) string { return r.PathValue("ns") + "/" + r.PathValue("name") }

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := s.readJSON(w, r, maxJSONBytes, v, true); err != nil {
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

// repoDetail is the body of GET /api/v1/repos/{repo}.
type repoDetail struct {
	registry.Repo
	Refs      []registry.Ref           `json:"refs"`
	Revisions []registry.CommitSummary `json:"revisions"`
}

// describeRepo gathers a repository with its refs and revisions.
func (s *Server) describeRepo(ctx context.Context, name string) (repoDetail, error) {
	repo, err := s.reg.GetRepo(ctx, name)
	if err != nil {
		return repoDetail{}, err
	}
	refs, err := s.reg.Refs(ctx, repo)
	if err != nil {
		return repoDetail{}, err
	}
	commits, err := s.reg.Commits(ctx, repo)
	if err != nil {
		return repoDetail{}, err
	}
	return repoDetail{Repo: repo, Refs: refs, Revisions: commits}, nil
}

func (s *Server) apiGetRepo(w http.ResponseWriter, r *http.Request) {
	d, err := s.describeRepo(r.Context(), repoParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// revisionDetail is the body of GET /api/v1/repos/{repo}/revisions/{rev}.
type revisionDetail struct {
	registry.Commit
	Files []registry.File `json:"files"`
}

func (s *Server) apiGetRevision(w http.ResponseWriter, r *http.Request) {
	c, err := s.resolve(r.Context(), repoParam(r), r.PathValue("rev"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, revisionDetail{Commit: c, Files: files})
}

// commitRequest is the body of POST /api/v1/repos/{repo}/commits.
type commitRequest struct {
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
	var req commitRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	if req.Branch == "" {
		req.Branch = defaultBranch
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

// freed is the answer to an operation that removes stored files: what was
// freed and, when some of it could not be, a warning saying so. The
// operation itself succeeded either way.
type freed struct {
	registry.GCResult
	Warning string `json:"warning,omitempty"`
}

// What a caller is told when files could not be removed from storage. The
// reason is in the server's log.
const (
	deleteWarning = "the repository was deleted, but some stored files that only it used could not be removed and still take up space; the server log has the reason"
	gcWarning     = "some unreferenced files could not be removed and still take up space; the server log has the reason"
)

// answerFreed reports res, with the warning if notFreed says some blobs
// could not be removed. The failure is the operator's to look into, so it
// is logged in full and not described to the caller.
func (s *Server) answerFreed(w http.ResponseWriter, res registry.GCResult, notFreed error, warning string) {
	out := freed{GCResult: res}
	if notFreed != nil {
		s.log.Warn("stored files could not be removed", "error", notFreed)
		out.Warning = warning
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	res, err := s.reg.DeleteRepo(r.Context(), repoParam(r))
	// The deletion is one transaction, and freeing the repository's files
	// comes after it. A failure there leaves the repository deleted, which
	// is what the caller asked for and is told.
	if err != nil && !errors.Is(err, registry.ErrBlobsNotFreed) {
		s.fail(w, r, err)
		return
	}
	s.log.Info("repository deleted", "repo", repoParam(r), "blobs_freed", res.Blobs, "bytes_freed", res.Bytes)
	s.answerFreed(w, res, err, deleteWarning)
}

func (s *Server) apiStatBlob(w http.ResponseWriter, r *http.Request) {
	// Asking whether a blob exists is what a push does before deciding not
	// to upload it, so it also marks the blob as in use.
	info, ok, err := s.reg.TouchBlob(r.Context(), r.PathValue("sha256"))
	if err != nil {
		// A HEAD answer has no body to explain itself in; the log does.
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) apiPutBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !blob.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "blob name must be a lowercase hex SHA-256")
		return
	}
	body := io.Reader(r.Body)
	if s.cfg.MaxBlobBytes > 0 {
		if over, err := s.blobTooLarge(r.ContentLength); over {
			s.fail(w, r, err)
			return
		}
		// A body sent without a declared length is cut off at the limit.
		body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBlobBytes)
	}
	// The body is streamed to disk and hashed as it arrives; the upload is
	// rejected unless it hashes to the digest in the URL.
	info, err := s.reg.PutBlob(r.Context(), body, blob.Expect{SHA256: digest, Size: r.ContentLength})
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			err = s.blobLimitError()
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": info.SHA256, "size": info.Size})
}

// mirrorRequest is the body of POST /api/v1/mirror.
type mirrorRequest struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"`
	// UpstreamToken authenticates to the upstream hub for private or gated
	// repositories. It is used for this request only and never stored.
	UpstreamToken string `json:"upstream_token"`
}

// mirrorFailedLocally is the last line of a mirror that failed for a
// reason of the registry's own. The reason is in the server's log.
const mirrorFailedLocally = "mirror failed: the registry could not store the revision; the server log has the reason"

func (s *Server) apiMirror(w http.ResponseWriter, r *http.Request) {
	var req mirrorRequest
	if !s.decodeBody(w, r, &req) {
		return
	}
	up := s.upstream()
	if req.UpstreamToken != "" {
		up.Token = req.UpstreamToken
	}
	// Resolve upstream before answering, so a wrong name or a missing
	// token comes back as a proper HTTP error.
	plan, err := mirror.Prepare(r.Context(), up, req.Repo, req.Revision)
	if err != nil {
		s.fail(w, r, upstreamFailure(err))
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
	started := s.now()
	// Progress callbacks arrive from several goroutines; serialise writes.
	events := make(chan mirror.Event, mirrorEventBuffer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e := range events {
			emit(e)
		}
	}()
	// The last event Execute sends on success is the "done" line.
	res, err := plan.Execute(r.Context(), s.reg, mirror.DefaultConcurrency, func(e mirror.Event) { events <- e })
	close(events)
	<-done
	if err != nil {
		emit(mirror.Event{Event: mirror.EventError, Error: s.mirrorFailure(plan, err)})
		return
	}
	s.log.Info("mirror complete", "repo", res.Repo, "sha", res.SHA, "files", res.Files,
		"downloaded_bytes", res.DownloadedBytes, "reused_bytes", res.ReusedBytes, "took", s.now().Sub(started).Round(time.Millisecond))
}

// upstreamFailure marks a failure to reach or read the upstream, of a kind
// that has no status of its own (it could not be reached, it answered with
// an error, its answer made no sense), so that a mirror request answers it
// as pull-through does: 502 with the reason. An error that already has a
// status (not found upstream, access denied, a bad name in the request)
// keeps it, and a failure that is not the upstream's stays the registry's
// own.
func upstreamFailure(err error) error {
	if status, _ := statusFor(err); status == http.StatusInternalServerError && mirror.FromUpstream(err) {
		return errUpstream{err}
	}
	return err
}

// mirrorFailure logs why a mirror failed and returns what its client is
// told, by the rule every other answer follows: a failure of the
// registry's own (its disk, its database) is logged in full and described
// no further, and anything else (the upstream's doing, or the request's)
// is passed on with its reason.
func (s *Server) mirrorFailure(plan *mirror.Plan, err error) string {
	status, _ := statusFor(err)
	switch {
	case mirror.FromUpstream(err) && status == http.StatusInternalServerError:
		// The upstream's doing, of a kind with no status of its own: a
		// dropped connection, an error answer.
		s.log.Warn("mirror failed", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
		return "mirror failed: " + err.Error()
	case status == http.StatusInternalServerError:
		s.log.Error("mirror failed", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
		return mirrorFailedLocally
	}
	s.log.Warn("mirror failed", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
	return err.Error()
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
	res, err := s.reg.GC(r.Context(), registry.BlobGrace)
	// Nothing freed and an error is a collection that did not happen. Some
	// freed and an error is one that did, with something left over.
	if err != nil && res.Blobs == 0 {
		s.fail(w, r, err)
		return
	}
	s.log.Info("garbage collected", "blobs_freed", res.Blobs, "bytes_freed", res.Bytes)
	s.answerFreed(w, res, err, gcWarning)
}

// The lines of a verify run's progress, in the order they are sent: one
// start, a blob for each stored file, then done or error.
const (
	verifyStart = "start"
	verifyBlob  = "blob"
	verifyDone  = "done"
	verifyError = "error"
)

// apiVerify re-reads every stored blob and streams what it finds as JSON
// lines: a "start" line, one "blob" line per blob as it is checked, and a
// final "done" line with the totals, or an "error" line if the run could
// not finish. Nothing is collected in memory, and nothing is locked: files
// are served as usual while it runs. Stopping the request stops the run.
func (s *Server) apiVerify(w http.ResponseWriter, r *http.Request) {
	blobs, bytes, err := s.reg.BlobTotals(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	emit := func(v any) {
		_ = enc.Encode(v)
		_ = rc.Flush()
	}
	emit(map[string]any{"event": verifyStart, "blobs": blobs, "bytes": bytes})
	started := s.now()
	res, err := s.reg.Verify(r.Context(), func(c registry.BlobCheck) {
		if c.Status != registry.BlobOK {
			s.log.Warn("stored file failed verification", "sha256", c.SHA256, "status", c.Status, "detail", c.Detail, "repos", c.Repos)
		}
		emit(struct {
			Event string `json:"event"`
			registry.BlobCheck
		}{verifyBlob, c})
	})
	if err != nil {
		s.log.Warn("verify did not finish", "checked", res.Blobs, "error", err)
		msg := "the check did not finish: " + err.Error()
		if status, _ := statusFor(err); status == http.StatusInternalServerError {
			msg = "the check did not finish; the server's log has the reason"
		}
		emit(map[string]any{"event": verifyError, "error": msg})
		return
	}
	s.log.Info("verify complete", "blobs", res.Blobs, "bytes", res.Bytes, "failed", res.Failed, "took", s.now().Sub(started).Round(time.Millisecond))
	emit(struct {
		Event string `json:"event"`
		registry.VerifyResult
	}{verifyDone, res})
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
		Name string        `json:"name"`
		Role registry.Role `json:"role"`
	}
	if !s.decodeBody(w, r, &req) {
		return
	}
	secret, err := s.reg.CreateToken(r.Context(), req.Name, req.Role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("token created", "name", req.Name, "role", req.Role)
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "role": string(req.Role), "token": secret})
}

func (s *Server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.RevokeToken(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("token revoked", "name", r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}
