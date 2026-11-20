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

// apiRepoRoot is where repositories of type typ live in the registry's API:
// /api/v1/repos for models, as before datasets existed, and
// /api/v1/datasets for datasets.
func apiRepoRoot(typ registry.RepoType) string {
	if typ == registry.TypeDataset {
		return "/api/v1/datasets"
	}
	return "/api/v1/repos"
}

// repoParam is the key of the repository of type typ that a route's
// {ns}/{name} names.
func repoParam(r *http.Request, typ registry.RepoType) string {
	return registry.RepoKey(typ, r.PathValue("ns")+"/"+r.PathValue("name"))
}

func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := s.readJSON(w, r, maxJSONBytes, v, true); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// apiListRepos lists the repositories of type typ.
func (s *Server) apiListRepos(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all, err := s.reg.ListRepos(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		repos := []registry.RepoSummary{}
		for _, repo := range all {
			if repo.Type == typ {
				repos = append(repos, repo)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
	}
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

func (s *Server) apiGetRepo(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d, err := s.describeRepo(r.Context(), repoParam(r, typ))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// revisionDetail is the body of GET /api/v1/repos/{repo}/revisions/{rev}.
type revisionDetail struct {
	registry.Commit
	Files []registry.File `json:"files"`
}

func (s *Server) apiGetRevision(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := s.resolve(r.Context(), repoParam(r, typ), r.PathValue("rev"))
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

func (s *Server) apiCommit(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req commitRequest
		if !s.decodeBody(w, r, &req) {
			return
		}
		if req.Branch == "" {
			req.Branch = defaultBranch
		}
		in := registry.CommitInput{Repo: repoParam(r, typ), Branch: req.Branch, Tags: req.Tags, Message: req.Message,
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
		s.m.commits.With(sourcePush).Inc()
		s.log.Info("revision recorded", "repo", c.Key(), "sha", c.SHA, "branch", req.Branch)
		writeJSON(w, http.StatusCreated, c)
	}
}

// uploaded counts a file that an upload sent and the registry stored.
func (s *Server) uploaded(info blob.Info) {
	s.m.uploadBlobs.Inc()
	s.m.uploadBytes.Add(info.Size)
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
		s.logNotFreed(notFreed)
		out.Warning = warning
	}
	writeJSON(w, http.StatusOK, out)
}

// logNotFreed logs why stored files could not be removed: one line for
// each file, naming it by its digest, so that none is hidden behind the
// first. attrs are added to every line.
func (s *Server) logNotFreed(notFreed error, attrs ...any) {
	var each *registry.NotFreedError
	if !errors.As(notFreed, &each) {
		s.log.Warn("stored files could not be removed", append(attrs, "error", notFreed)...)
		return
	}
	for _, failure := range each.Failures {
		s.log.Warn("stored files could not be removed", append(attrs[:len(attrs):len(attrs)], "error", failure)...)
	}
}

func (s *Server) apiDeleteRepo(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repo := repoParam(r, typ)
		res, err := s.reg.DeleteRepo(r.Context(), repo)
		// The deletion is one transaction, and freeing the repository's files
		// comes after it. A failure there leaves the repository deleted, which
		// is what the caller asked for and is told.
		if err != nil && !errors.Is(err, registry.ErrBlobsNotFreed) {
			s.fail(w, r, err)
			return
		}
		s.log.Info("repository deleted", "repo", repo, "blobs_freed", res.Blobs, "bytes_freed", res.Bytes)
		s.answerFreed(w, res, err, deleteWarning)
	}
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
		body = http.MaxBytesReader(baseWriter(w), r.Body, s.cfg.MaxBlobBytes)
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
	s.uploaded(info)
	writeJSON(w, http.StatusCreated, map[string]any{"sha256": info.SHA256, "size": info.Size})
}

// mirrorRequest is the body of POST /api/v1/mirror.
type mirrorRequest struct {
	Repo string `json:"repo"`
	// Type is "dataset" for a dataset; empty or "model" for a model.
	Type     registry.RepoType `json:"type"`
	Revision string            `json:"revision"`
	// AllRefs mirrors every branch and tag the upstream has, in place of
	// one revision.
	AllRefs bool `json:"all_refs"`
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
	typ, err := registry.ParseRepoType(string(req.Type))
	if err == nil {
		// The name on its own first: one of more segments must not pass for
		// another type's key.
		err = registry.ValidateRepoName(req.Repo)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	up := s.upstream()
	if req.UpstreamToken != "" {
		up.Token = req.UpstreamToken
	}
	if req.AllRefs {
		s.mirrorAllRefs(w, r, up, registry.RepoKey(typ, req.Repo), req.Revision)
		return
	}
	// Resolve upstream before answering, so a wrong name or a missing
	// token comes back as a proper HTTP error.
	plan, err := mirror.Prepare(r.Context(), up, registry.RepoKey(typ, req.Repo), req.Revision)
	if err != nil {
		s.fail(w, r, upstreamFailure(err))
		return
	}

	emit, events, done := s.mirrorStream(w)
	started := s.now()
	// The last event Execute sends on success is the "done" line.
	s.m.mirrorsStarted.Inc()
	res, err := plan.Execute(r.Context(), s.reg, mirror.DefaultConcurrency, func(e mirror.Event) { events <- e })
	close(events)
	<-done
	if err != nil {
		s.m.mirrors.With(outcomeFailed).Inc()
		emit(mirror.Event{Event: mirror.EventError, Error: s.mirrorFailure(plan.Repo, plan.Info.SHA, err)})
		return
	}
	s.m.mirrors.With(outcomeSucceeded).Inc()
	s.m.mirrorBytes.Add(res.DownloadedBytes)
	s.m.commits.With(sourceMirror).Inc()
	s.log.Info("mirror complete", "repo", res.Repo, "sha", res.SHA, "files", res.Files,
		"downloaded_bytes", res.DownloadedBytes, "reused_bytes", res.ReusedBytes, "took", s.now().Sub(started).Round(time.Millisecond))
}

// mirrorStream starts the answer to a mirror request that has begun: a
// stream of JSON lines. emit writes one line; events takes progress lines
// from any goroutine and writes them in turn, until it is closed, after
// which done is closed.
func (s *Server) mirrorStream(w http.ResponseWriter) (emit func(any), events chan<- mirror.Event, done <-chan struct{}) {
	// From here the response is a stream of JSON lines; failures are
	// reported as a final {"event":"error"} line.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	emit = func(e any) {
		_ = enc.Encode(e)
		_ = rc.Flush()
	}
	// Progress callbacks arrive from several goroutines; serialise writes.
	queue := make(chan mirror.Event, mirrorEventBuffer)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for e := range queue {
			emit(e)
		}
	}()
	return emit, queue, finished
}

// mirrorAllRefs answers a mirror request for every branch and tag of the
// repository with key key. The upstream's refs are listed before the
// answer starts, so that a wrong name or a missing token is an ordinary
// error; after that every ref is reported on a line of its own, and the
// done line carries them all. A ref that failed is reported there and does
// not undo the others.
func (s *Server) mirrorAllRefs(w http.ResponseWriter, r *http.Request, up *mirror.Upstream, key, revision string) {
	if revision != "" {
		writeError(w, http.StatusBadRequest, "all_refs mirrors every branch and tag; leave revision out")
		return
	}
	listed, err := mirror.ListRefs(r.Context(), up, key, nil)
	if err != nil {
		s.fail(w, r, upstreamFailure(err))
		return
	}
	emit, events, done := s.mirrorStream(w)
	started := s.now()
	s.m.mirrorsStarted.Inc()
	res, err := mirror.MirrorRefs(r.Context(), up, s.reg, key, registry.FollowSpec{AllRefs: true}, listed, mirror.Options{
		Concurrency: mirror.DefaultConcurrency,
		Progress:    func(e mirror.Event) { events <- e },
		Describe:    func(err error) string { return s.mirrorFailure(key, "", err) },
	})
	close(events)
	<-done
	if err != nil {
		s.m.mirrors.With(outcomeFailed).Inc()
		emit(mirror.Event{Event: mirror.EventError, Error: s.mirrorFailure(key, "", err)})
		return
	}
	outcome := outcomeSucceeded
	if len(res.Failed()) > 0 {
		outcome = outcomeFailed
	}
	s.m.mirrors.With(outcome).Inc()
	s.m.mirrorBytes.Add(res.DownloadedBytes)
	if res.Changed() {
		s.m.commits.With(sourceMirror).Inc()
	}
	s.log.Info("mirror of every ref complete", "repo", key, "refs", len(res.Refs), "failed", len(res.Failed()),
		"downloaded_bytes", res.DownloadedBytes, "reused_bytes", res.ReusedBytes, "took", s.now().Sub(started).Round(time.Millisecond))
	emit(res.DoneEvent())
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
func (s *Server) mirrorFailure(repo, sha string, err error) string {
	status, _ := statusFor(err)
	switch {
	case mirror.FromUpstream(err) && status == http.StatusInternalServerError:
		// The upstream's doing, of a kind with no status of its own: a
		// dropped connection, an error answer.
		s.log.Warn("mirror failed", "repo", repo, "sha", sha, "error", err)
		return "mirror failed: " + err.Error()
	case status == http.StatusInternalServerError:
		s.log.Error("mirror failed", "repo", repo, "sha", sha, "error", err)
		return mirrorFailedLocally
	}
	s.log.Warn("mirror failed", "repo", repo, "sha", sha, "error", err)
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

// collected reports whether a garbage collection that ended with err took
// place all the same. An error with nothing removed, and nothing that
// storage refused to remove, is a collection that did not happen. Otherwise
// it happened, with something left over: files storage would not let go
// of, which stay recorded and are tried again by a later collection.
func collected(res registry.GCResult, err error) bool {
	var notFreed *registry.NotFreedError
	return res.Blobs > 0 || (errors.As(err, &notFreed) && notFreed.Stuck > 0)
}

func (s *Server) apiGC(w http.ResponseWriter, r *http.Request) {
	res, err := s.reg.GC(r.Context(), registry.BlobGrace)
	if err != nil && !collected(res, err) {
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
