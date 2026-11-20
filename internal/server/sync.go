// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// Keeping mirrors up to date: a followed repository (registry.Follow) is
// checked against its upstream, on a schedule or when asked, and whatever
// has moved there is mirrored.
//
// A check, step by step:
//
//  1. Ask the upstream for the repository's branches and tags (one small
//     request, on a listing slot).
//  2. For each followed ref, compare it with the registry's: one at the
//     same revision with every file stored is left alone; a new one, or a
//     branch that moved, has its revision mirrored in full, fetching only
//     the files the registry does not already store, and only then does
//     the ref move (mirror.MirrorRefs). A tag that moved upstream is
//     reported and left; a ref deleted upstream stays.
//  3. Record how it went against the repository: when, whether anything
//     changed, and the failure if there was one.
//
// What bounds it:
//
//   - One check runs at a time (maxConcurrentSyncs), and it fetches one
//     file at a time, holding one of the slots pull-through fetches use for
//     each. So a sync never has more than one file in flight, the registry
//     never more upstream fetches than it has slots, and three of the four
//     stay for the downloads clients are waiting on.
//   - The schedule spreads the checks: each repository has its own place
//     within the interval (syncPhase), and repositories due at once take
//     their turns.
//   - A repository whose checks keep failing is checked less often, twice
//     the wait after each failure up to maxSyncBackoff, while the others
//     go on as before.
//
// Like a pull-through fetch, a check belongs to the server, not to the
// request that asked for it: it runs on the server's lifetime, a client
// that hangs up does not stop it, and Server.Shutdown cancels it and waits.

const (
	// maxConcurrentSyncs is how many followed repositories are checked at
	// once.
	maxConcurrentSyncs = 1
	// syncFileConcurrency is how many files one check fetches at once.
	syncFileConcurrency = 1
	// syncTickPeriod is how often the schedule looks for a repository that
	// is due, when the interval is longer than that.
	syncTickPeriod = time.Minute
	// maxSyncBackoff is the longest a failing repository waits between
	// checks, unless the interval itself is longer.
	maxSyncBackoff = 24 * time.Hour
	// syncFailedLocally is what is recorded, and told, when a check failed
	// for a reason of the registry's own. The log has the rest.
	syncFailedLocally = "the registry could not store what it fetched; the server log has the reason"
)

// syncWait is how long after a check the next one is due: the interval,
// doubled for each failure in a row, up to maxSyncBackoff (or the interval,
// if that is longer).
func syncWait(failures int, interval time.Duration) time.Duration {
	limit := max(maxSyncBackoff, interval)
	wait := interval
	for i := 0; i < failures && wait < limit; i++ {
		wait *= 2
	}
	return min(wait, limit)
}

// syncPhase is the repository's own place within the interval, the same
// for it every time: the schedule does not check a repository before this
// much of the first interval after the server started has passed, so that
// repositories followed together, or overdue after a restart, are not all
// checked at once.
func syncPhase(key string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return time.Duration(h.Sum64() % uint64(interval)) //nolint:gosec // less than interval, which is a positive int64.
}

// syncDue is when the schedule next checks f, for a server whose schedule
// started at started.
func syncDue(f registry.Follow, interval time.Duration, started time.Time) time.Time {
	due := started.Add(syncPhase(f.Key(), interval))
	if !f.LastCheck.IsZero() {
		if next := f.LastCheck.Add(syncWait(f.Failures, interval)); next.After(due) {
			due = next
		}
	}
	return due
}

// syncResult is what one check did: the body of
// POST /api/v1/repos/{repo}/sync.
type syncResult struct {
	Name      string            `json:"name"`
	Type      registry.RepoType `json:"type,omitzero"`
	CheckedAt time.Time         `json:"checked_at"`
	// Changed is set when a ref was added or moved.
	Changed         bool               `json:"changed"`
	Refs            []mirror.RefResult `json:"refs"`
	DownloadedBytes int64              `json:"downloaded_bytes"`
	// Error is why the check failed, as it was recorded.
	Error string `json:"error,omitempty"`
	// err is the failure itself, which decides the status of the answer.
	err error
}

// syncJob is one check in progress, shared by everyone who asks for it
// while it runs. result is set before done is closed and read only after.
type syncJob struct {
	done   chan struct{}
	result syncResult
}

// syncer runs the checks of followed repositories: those the schedule
// finds due, and those asked for.
type syncer struct {
	s *Server
	// ctx is the server's lifetime; every check runs on it.
	ctx context.Context
	// interval is the schedule's; 0 when there is none, and checks run
	// only when asked for.
	interval time.Duration
	// started is when the schedule started, by the server's clock.
	started time.Time
	// turns lets maxConcurrentSyncs checks run at once.
	turns chan struct{}
	// files and listings are the upstream slots the server shares between
	// pull-through and syncs (upstreamSlots).
	files, listings mirror.Slot

	mu   sync.Mutex
	jobs map[string]*syncJob // by repository key
	// running counts the checks and the schedule's loop that have not
	// ended; drained is closed once closed is set and it is zero.
	running int
	closed  bool
	drained chan struct{}
	allGone sync.Once
}

func newSyncer(s *Server, lifetime context.Context, interval time.Duration, slots upstreamSlots) *syncer {
	return &syncer{
		s: s, ctx: lifetime, interval: interval, started: s.now(),
		turns: make(chan struct{}, maxConcurrentSyncs),
		files: slots.file, listings: slots.listing,
		jobs: map[string]*syncJob{}, drained: make(chan struct{}),
	}
}

// begin counts one more piece of work, unless the server is stopping.
func (y *syncer) begin() bool {
	y.mu.Lock()
	defer y.mu.Unlock()
	if y.closed {
		return false
	}
	y.running++
	return true
}

// end is the last thing a piece of work does.
func (y *syncer) end(forget func()) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if forget != nil {
		forget()
	}
	y.running--
	if y.closed && y.running == 0 {
		y.allGone.Do(func() { close(y.drained) })
	}
}

// schedule runs the schedule: on each tick it starts a check of every
// followed repository that is due. It returns when the server stops.
func (y *syncer) schedule(ticks <-chan time.Time, stopTicker func()) {
	if !y.begin() {
		stopTicker()
		return
	}
	go func() {
		defer y.end(nil)
		defer stopTicker()
		for {
			select {
			case <-y.ctx.Done():
				return
			case <-ticks:
				y.startDue()
			}
		}
	}()
}

// startDue starts a check of each followed repository that is due now. A
// repository already being checked is not started again.
func (y *syncer) startDue() {
	follows, err := y.s.reg.Follows(y.ctx)
	if err != nil {
		if y.ctx.Err() == nil {
			y.s.log.Error("sync: could not read the follow list", "error", err)
		}
		return
	}
	now := y.s.now()
	for _, f := range follows {
		if !now.Before(syncDue(f, y.interval, y.started)) {
			_, _ = y.start(f.Key())
		}
	}
}

// start starts a check of the repository with key key, or joins the one in
// progress, and returns it without waiting.
func (y *syncer) start(key string) (*syncJob, error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	if job := y.jobs[key]; job != nil {
		return job, nil
	}
	if y.closed {
		return nil, errShuttingDown
	}
	y.running++
	job := &syncJob{done: make(chan struct{})}
	y.jobs[key] = job
	// On the server's lifetime: a check outlives the request that asked
	// for it, and is stopped by Shutdown.
	go func() {
		job.result = y.run(key)
		y.end(func() {
			delete(y.jobs, key)
			close(job.done)
		})
	}()
	return job, nil
}

// run waits for a turn and checks the repository with key key.
func (y *syncer) run(key string) syncResult {
	typ, name := registry.SplitRepoKey(key)
	res := syncResult{Name: name, Type: typ}
	if typ == registry.TypeModel {
		res.Type = ""
	}
	select {
	case y.turns <- struct{}{}:
		defer func() { <-y.turns }()
	case <-y.ctx.Done():
		res.err = errShuttingDown
		return res
	}
	if y.ctx.Err() != nil {
		res.err = errShuttingDown
		return res
	}
	res.CheckedAt = y.s.now()
	started := time.Now() // a duration is real time, not the server's clock
	refs, err := y.check(key)
	res.Refs, res.Changed, res.DownloadedBytes = refs.Refs, refs.Changed(), refs.DownloadedBytes
	if y.ctx.Err() != nil {
		// The server stopped under it. Nothing is recorded: the check did
		// not fail, it was not finished.
		res.err = errShuttingDown
		return res
	}
	res.err = err
	if errors.Is(err, registry.ErrNotFollowed) {
		return res // taken off the list while it waited for its turn
	}
	cause := ""
	if err != nil {
		res.Error, cause = y.failure(key, err)
	}
	for _, r := range res.Refs {
		if r.Outcome == mirror.RefTagMoved || r.Outcome == mirror.RefConflict {
			y.s.log.Warn("sync: a ref was left as it is", "repo", key, "ref", r.Name, "outcome", r.Outcome, "detail", r.Detail)
		}
	}
	m := &y.s.m
	m.syncChecks.Inc()
	if res.Changed {
		m.syncChanges.Inc()
	}
	if cause != "" {
		m.syncFailures.With(cause).Inc()
	}
	m.syncBytes.Add(res.DownloadedBytes)
	if err := y.s.reg.RecordSync(y.ctx, key, registry.SyncRecord{At: res.CheckedAt, Changed: res.Changed, Error: res.Error}); err != nil {
		y.s.log.Error("sync: could not record how the check went", "repo", key, "error", err)
	}
	y.s.log.Info("followed repository checked", "repo", key, "changed", res.Changed, "failed", res.Error != "",
		"downloaded_bytes", res.DownloadedBytes, "took", time.Since(started).Round(time.Millisecond))
	return res
}

// check asks the upstream about the followed refs of the repository with
// key key and mirrors what has moved. The error is the check's failure: the
// upstream could not be asked, a ref failed, or the followed branch is not
// upstream any more.
func (y *syncer) check(key string) (mirror.RefsResult, error) {
	f, err := y.s.reg.GetFollow(y.ctx, key)
	if err != nil {
		return mirror.RefsResult{}, err
	}
	up := y.s.upstream()
	listed, err := mirror.ListRefs(y.ctx, up, key, y.listings)
	if err != nil {
		return mirror.RefsResult{}, err
	}
	res, err := mirror.MirrorRefs(y.ctx, up, y.s.reg, key, f.Spec(), listed, mirror.Options{
		Concurrency: syncFileConcurrency, FileSlot: y.files, ListingSlot: y.listings,
		Describe: y.describe,
	})
	if err != nil {
		return res, err
	}
	var failed []error
	for _, r := range res.Refs {
		switch {
		case r.Outcome == mirror.RefFailed:
			failed = append(failed, refError{name: r.Name, err: r.Err})
		case !f.AllRefs && r.Outcome == mirror.RefDeletedUpstream:
			failed = append(failed, refError{name: r.Name, err: fmt.Errorf("%w: the followed branch %q is not upstream any more; the registry's is left as it is",
				mirror.ErrUpstreamNotFound, r.Name)})
		}
	}
	return res, errors.Join(failed...)
}

// refError is the failure of one ref of a check.
type refError struct {
	name string
	err  error
}

func (e refError) Error() string { return e.name + ": " + e.err.Error() }
func (e refError) Unwrap() error { return e.err }

// describe is what a failed ref is reported as: the reason when the
// upstream caused it, and no more than that it was the registry's own
// failure otherwise.
func (y *syncer) describe(err error) string {
	if syncCause(err) == outcomeLocal {
		return syncFailedLocally
	}
	return err.Error()
}

// failure logs a failed check and returns what is recorded against the
// repository and the cause it is counted under. A failure of the
// registry's own is logged in full and recorded as no more than that.
func (y *syncer) failure(key string, err error) (msg, cause string) {
	cause = syncCause(err)
	if cause == outcomeLocal {
		y.s.log.Error("sync failed", "repo", key, "error", err)
		return syncFailedLocally, cause
	}
	y.s.log.Warn("sync failed", "repo", key, "cause", cause, "error", err)
	return err.Error(), cause
}

// syncCause says what a failed check is counted as. Of several failed refs
// the first decides.
func syncCause(err error) string {
	var multi interface{ Unwrap() []error }
	if errors.As(err, &multi) && len(multi.Unwrap()) > 0 {
		err = multi.Unwrap()[0]
	}
	switch {
	case errors.Is(err, blob.ErrMismatch):
		return outcomeVerification
	case errors.Is(err, mirror.ErrUpstreamNotFound):
		return outcomeNotFound
	case mirror.FromUpstream(err):
		return outcomeUpstream
	}
	return outcomeLocal
}

// wait stops new checks from starting and returns when every check and the
// schedule have ended, or with ctx's error. The caller cancels the
// lifetime first (Server.Shutdown).
func (y *syncer) wait(ctx context.Context) error {
	y.mu.Lock()
	y.closed = true
	left := y.running
	if left == 0 {
		y.allGone.Do(func() { close(y.drained) })
	}
	y.mu.Unlock()
	select {
	case <-y.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%d syncs had not stopped: %w", left, ctx.Err())
	}
}

// syncAnswer is the error a check that failed is answered with: what the
// upstream caused is a 502 with the reason, the registry's own failure a
// 500, logged already.
func syncAnswer(res syncResult) error {
	switch {
	case res.err == nil:
		return nil
	case errors.Is(res.err, errShuttingDown), errors.Is(res.err, registry.ErrNotFollowed):
		return res.err
	case syncCause(res.err) == outcomeLocal:
		return errInternal{res.err}
	}
	return errUpstream{res.err}
}

// ---- API ----

// followRequest is the body of POST /api/v1/repos/{repo}/follow.
type followRequest struct {
	// Branch is the branch to follow. Empty, with AllRefs unset, means the
	// repository's only branch, or main.
	Branch  string `json:"branch"`
	AllRefs bool   `json:"all_refs"`
}

// followEntry is a follow list entry as the API shows it.
type followEntry struct {
	registry.Follow
	// NextCheck is when the schedule next checks it; absent with no
	// schedule.
	NextCheck time.Time `json:"next_check,omitzero"`
}

func (s *Server) followEntry(f registry.Follow) followEntry {
	e := followEntry{Follow: f}
	if s.syncer.interval > 0 {
		e.NextCheck = syncDue(f, s.syncer.interval, s.syncer.started)
	}
	return e
}

// defaultFollowBranch is the branch a follow request that names none
// means: the repository's only branch, or main.
func (s *Server) defaultFollowBranch(ctx context.Context, key string) (string, error) {
	repo, err := s.reg.GetRepo(ctx, key)
	if err != nil {
		return "", err
	}
	refs, err := s.reg.Refs(ctx, repo)
	if err != nil {
		return "", err
	}
	var branches []string
	for _, r := range refs {
		if r.Kind == registry.KindBranch {
			branches = append(branches, r.Name)
		}
	}
	switch {
	case len(branches) == 1:
		return branches[0], nil
	case len(branches) > 1:
		for _, b := range branches {
			if b == defaultBranch {
				return b, nil
			}
		}
		return "", fmt.Errorf("%w: %s has the branches %s; name the one to follow, or follow every branch and tag",
			registry.ErrInvalid, key, strings.Join(branches, ", "))
	}
	return defaultBranch, nil
}

func (s *Server) apiFollow(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req followRequest
		if !s.decodeBody(w, r, &req) {
			return
		}
		key := repoParam(r, typ)
		if req.Branch == "" && !req.AllRefs {
			branch, err := s.defaultFollowBranch(r.Context(), key)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			req.Branch = branch
		}
		f, err := s.reg.Follow(r.Context(), key, registry.FollowSpec{Branch: req.Branch, AllRefs: req.AllRefs})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.log.Info("repository followed", "repo", key, "branch", f.Branch, "all_refs", f.AllRefs)
		writeJSON(w, http.StatusOK, s.followEntry(f))
	}
}

func (s *Server) apiUnfollow(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := repoParam(r, typ)
		if err := s.reg.Unfollow(r.Context(), key); err != nil {
			s.fail(w, r, err)
			return
		}
		s.log.Info("repository no longer followed", "repo", key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiListFollows(w http.ResponseWriter, r *http.Request) {
	follows, err := s.reg.Follows(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]followEntry, len(follows))
	for i, f := range follows {
		out[i] = s.followEntry(f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"follows": out, "interval_seconds": int64(s.syncer.interval / time.Second)})
}

// apiSyncNow checks one followed repository now and answers when the check
// is over. A client that hangs up does not stop it.
func (s *Server) apiSyncNow(typ registry.RepoType) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := repoParam(r, typ)
		if _, err := s.reg.GetFollow(r.Context(), key); err != nil {
			s.fail(w, r, err)
			return
		}
		job, err := s.syncer.start(key)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		select {
		case <-job.done:
		case <-r.Context().Done():
			return // the check carries on; nobody is left to tell
		}
		if err := syncAnswer(job.result); err != nil {
			status, _, msg := s.failure(w, r, err)
			if errors.Is(err, errShuttingDown) || errors.Is(err, registry.ErrNotFollowed) {
				writeError(w, status, msg)
				return
			}
			// What failed is in the body with every ref's outcome, under
			// "error" as in any failure.
			res := job.result
			if status != http.StatusInternalServerError {
				res.Error = msg
			}
			writeJSON(w, status, res)
			return
		}
		writeJSON(w, http.StatusOK, job.result)
	}
}

// apiSyncAll starts a check of every followed repository and answers at
// once; they take their turns as the scheduled ones do.
func (s *Server) apiSyncAll(w http.ResponseWriter, r *http.Request) {
	follows, err := s.reg.Follows(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	queued := []string{}
	for _, f := range follows {
		if _, err := s.syncer.start(f.Key()); err != nil {
			s.fail(w, r, err)
			return
		}
		queued = append(queued, f.Key())
	}
	s.log.Info("sync of every followed repository asked for", "repos", len(queued))
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued})
}
