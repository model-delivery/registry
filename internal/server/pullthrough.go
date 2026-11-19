// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// Pull-through caching: when a client asks for something the registry does
// not hold, get it from the upstream hub, store it, and then answer.
//
// It works at two levels, and neither fetches more than was asked for:
//
//   - A revision the registry has never seen is recorded from the upstream's
//     file listing: every path, size and digest, and no content. That is one
//     small request, so listings and file metadata answer at once.
//   - A file's content is fetched the first time a client downloads that
//     file, verified against the digest in the listing, and stored. A
//     client that wants one file of a large repository costs one file.
//
// Three properties matter:
//
//   - A fetch belongs to the server, not to the request that started it. A
//     large file takes minutes, and clients give up sooner than that. If the
//     client disconnects the fetch carries on, and the next request for the
//     same thing joins it instead of starting another. What does end a fetch
//     is the server stopping: Server.Shutdown cancels every job and waits
//     for it, so none is still writing when the registry is closed.
//   - Nothing is served unverified. A file is only handed to a client once
//     all of it is stored and has matched its digest.
//   - What the registry already holds is served as is. Pull-through fills
//     gaps; it never refreshes a branch that has moved upstream.
//     `modelctl mirror` does that, and also fetches a whole revision at
//     once, which is what guarantees it is available offline.

// errUpstream wraps a pull-through failure that is the upstream's or the
// network's doing, so it is reported as a gateway error, not as "not found".
// Only a failure mirror.FromUpstream vouches for is wrapped in it; one of
// the registry's own is an errInternal.
type errUpstream struct{ err error }

func (e errUpstream) Error() string { return "could not fetch from upstream: " + e.err.Error() }
func (e errUpstream) Unwrap() error { return e.err }

// errPullBusy refuses a new fetch while too many are already waiting.
var errPullBusy = errors.New("the registry is busy fetching other files from upstream; try again in a minute")

// errShuttingDown is what a fetch ends with when the server stops under it,
// and what a request that would start one is told from then on.
var errShuttingDown = errors.New("the registry is shutting down; try again in a minute")

// errNotFromUpstream refuses to fetch into a repository that was created in
// this registry (by a push or an upload) and not from the upstream.
var errNotFromUpstream = errors.New("this repository was not created from the upstream")

// errFileTooLarge refuses a file over the operator's pull-through limit.
type errFileTooLarge struct {
	repo, path  string
	size, limit int64
}

func (e errFileTooLarge) Error() string {
	return fmt.Sprintf("%s is %s, more than this registry fetches on demand (REGISTRY_PULL_THROUGH_MAX_FILE_BYTES is %d): "+
		"raise that setting, or fetch the file ahead of time with `modelctl mirror %s`", e.path, kit.HumanBytes(e.size), e.limit, e.repo)
}

const (
	// How long an upstream "not found" is remembered, so a client retrying
	// a wrong name does not turn into a stream of upstream requests.
	missTTL = time.Minute
	// How many remembered misses are kept before they are all dropped. It
	// bounds memory; forgetting them early only costs an upstream request.
	maxRememberedMisses = 10_000
	// How many file fetches may run at once.
	maxConcurrentPulls = 4
	// How many revision listings may be asked for at once. A listing is one
	// small request, and it has slots of its own so that it never waits
	// behind file fetches, which can each run for hours.
	maxConcurrentListings = 8
	// How many more jobs may wait for a slot. A job outlives the request
	// that started it, so without a bound a client could queue any number
	// of them by asking and hanging up.
	maxWaitingPulls = 64
	// The most time one job may take, once it has a slot, before it is
	// abandoned.
	pullTimeout = 12 * time.Hour
	// The longest "repository@revision" that is looked up or remembered as a
	// miss. Valid names are shorter; this bounds what a request can make the
	// registry keep.
	maxPullKey = 512
)

type puller struct {
	reg      *registry.Registry
	upstream func() *mirror.Upstream
	allow    []string
	// maxFileBytes is the largest file fetched on demand; 0 means no limit.
	maxFileBytes int64
	log          *slog.Logger
	// ctx is the lifetime every job runs on: the server's, which ends at
	// Server.Shutdown. It is never a request's.
	ctx context.Context
	now func() time.Time

	mu sync.Mutex
	// The jobs in progress, of each kind, by what they fetch. A file fetch
	// yields nothing but success or failure; a listing yields the id the
	// revision was stored under.
	fileJobs    map[string]*pullJob[struct{}]
	listingJobs map[string]*pullJob[string]
	running     int // jobs of either kind that have not finished
	waiting     int // of those, the ones that do not hold a slot yet
	misses      map[string]time.Time
	// closed is set when the server stops: no job starts after it.
	closed bool
	// drained is closed once closed is set and no job is left.
	drained  chan struct{}
	allGone  sync.Once
	slots    chan struct{} // file fetches in progress
	listings chan struct{} // revision listings in progress
}

// pullJob is one fetch in progress, shared by every request that wants
// what it fetches. result and err are what its work returned; they are set
// by the job's own goroutine before done is closed and are read only after.
type pullJob[T any] struct {
	done   chan struct{}
	result T
	err    error
}

// newPuller builds a coordinator whose jobs run until they finish. The
// server gives it its own lifetime and clock (see New).
func newPuller(reg *registry.Registry, upstream func() *mirror.Upstream, allow []string, maxFileBytes int64, log *slog.Logger) *puller {
	return &puller{
		reg: reg, upstream: upstream, allow: allow, maxFileBytes: maxFileBytes, log: log,
		ctx: context.Background(), now: time.Now,
		fileJobs: map[string]*pullJob[struct{}]{}, listingJobs: map[string]*pullJob[string]{},
		misses:   map[string]time.Time{},
		drained:  make(chan struct{}),
		slots:    make(chan struct{}, maxConcurrentPulls),
		listings: make(chan struct{}, maxConcurrentListings),
	}
}

// allowed reports whether repo may be fetched on demand. With no patterns
// configured, every repository may. A pattern is a glob over the whole id,
// for example "meta-llama/*" or "*/*-GGUF".
func (p *puller) allowed(repo string) bool {
	if len(p.allow) == 0 {
		return true
	}
	for _, pattern := range p.allow {
		if ok, _ := path.Match(pattern, repo); ok {
			return true
		}
	}
	return false
}

// share runs a file fetch once per key however many callers ask for it at
// the same time, on the server's lifetime, and lets each caller wait only
// as long as its own ctx lives. A new job is refused with errPullBusy while
// maxWaitingPulls others are waiting for a slot; joining a job that already
// exists is never refused.
func (p *puller) share(ctx context.Context, key string, work func(ctx context.Context) error) error {
	_, err := shareJob(ctx, p, p.fileJobs, p.slots, key, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, work(ctx)
	})
	return err
}

// shareListing is share for a revision listing, which takes its slot from
// the listings' own pool and yields the id the revision is stored under.
func (p *puller) shareListing(ctx context.Context, key string, work func(ctx context.Context) (string, error)) (string, error) {
	return shareJob(ctx, p, p.listingJobs, p.listings, key, work)
}

// shareJob is what share and shareListing have in common: it joins the job
// for key among jobs, or starts one that takes its slot from pool, and
// waits for the result as long as ctx lives.
func shareJob[T any](ctx context.Context, p *puller, jobs map[string]*pullJob[T], pool chan struct{}, key string, work func(ctx context.Context) (T, error)) (T, error) {
	var none T
	p.mu.Lock()
	job := jobs[key]
	if job == nil {
		switch {
		case p.closed:
			p.mu.Unlock()
			return none, errShuttingDown
		case p.waiting >= maxWaitingPulls:
			p.mu.Unlock()
			return none, errPullBusy
		}
		p.waiting++
		p.running++
		job = &pullJob[T]{done: make(chan struct{})}
		jobs[key] = job
		// Deliberately not the request's context: the fetch must outlive the
		// request that started it (see the note at the top of this file).
		go func() {
			job.err = p.do(pool, func(ctx context.Context) (err error) {
				job.result, err = work(ctx)
				return err
			})
			p.finish(func() {
				delete(jobs, key)
				close(job.done)
			})
		}()
	}
	p.mu.Unlock()

	select {
	case <-job.done:
		return job.result, job.err
	case <-ctx.Done():
		return none, ctx.Err()
	}
}

// do is the body of a job's goroutine: wait for a slot in pool, then run
// work on the server's lifetime with the job's time limit.
//
// TestServeStopsItsOwnWorkBeforeTheRegistryCloses finds the jobs still
// running by this method's name in their stacks; rename it there too.
func (p *puller) do(pool chan struct{}, work func(ctx context.Context) error) error {
	select {
	case pool <- struct{}{}:
		defer func() { <-pool }()
	case <-p.ctx.Done():
	}
	p.mu.Lock()
	p.waiting--
	p.mu.Unlock()
	if p.ctx.Err() != nil {
		return errShuttingDown
	}
	// The time limit starts here, with the slot in hand: time spent queued
	// behind other jobs does not count against this one.
	ctx, cancel := context.WithTimeout(p.ctx, pullTimeout)
	defer cancel()
	err := work(ctx)
	if err != nil && p.ctx.Err() != nil {
		// Whatever the work failed with, the reason is that the server stopped.
		return errShuttingDown
	}
	return err
}

// finish takes a job off the books. forget removes it from its map and
// releases whoever waits on it; it runs under the lock, so a request that
// finds no job starts a new one and never joins one that has ended. It is
// the last thing a job's goroutine does.
func (p *puller) finish(forget func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	forget()
	p.running--
	if p.closed && p.running == 0 {
		p.allGone.Do(func() { close(p.drained) })
	}
}

// wait stops new jobs from starting and returns when the last one running
// has finished, or with ctx's error. The caller cancels the jobs' lifetime
// first (Server.Shutdown), or this waits for them to run to the end.
func (p *puller) wait(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	left := p.running
	if left == 0 {
		p.allGone.Do(func() { close(p.drained) })
	}
	p.mu.Unlock()
	select {
	case <-p.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%d pull-through fetches had not stopped: %w", left, ctx.Err())
	}
}

// ensure records repo@rev from the upstream's listing if the registry does
// not hold it. It returns the id the revision is stored under, which is
// the upstream's canonical id and can differ from repo after a rename.
func (p *puller) ensure(ctx context.Context, repo, rev string) (string, error) {
	key := repo + "@" + rev
	if len(key) > maxPullKey {
		return "", fmt.Errorf("%w: %s", mirror.ErrUpstreamNotFound, repo)
	}
	p.mu.Lock()
	if until, missed := p.misses[key]; missed {
		if p.now().Before(until) {
			p.mu.Unlock()
			return "", fmt.Errorf("%w: %s@%s", mirror.ErrUpstreamNotFound, repo, rev)
		}
		delete(p.misses, key)
	}
	p.mu.Unlock()

	return p.shareListing(ctx, key, func(ctx context.Context) (string, error) {
		// A request that missed locally just before another job recorded
		// this revision arrives here after that job has gone. Look again
		// before asking the upstream a second time.
		if c, err := p.reg.Resolve(ctx, repo, rev); err == nil {
			return c.Repo, nil
		}
		plan, err := mirror.Prepare(ctx, p.upstream(), repo, rev)
		if err != nil {
			if errors.Is(err, mirror.ErrUpstreamNotFound) {
				p.mu.Lock()
				if len(p.misses) > maxRememberedMisses {
					clear(p.misses)
				}
				p.misses[key] = p.now().Add(missTTL)
				p.mu.Unlock()
				return "", err
			}
			p.log.Warn("pull-through could not resolve upstream", "repo", repo, "revision", rev, "error", err)
			return "", err
		}
		// The upstream answers with its canonical id, which differs from
		// what was asked for after a rename. The allow list covers the name
		// the revision is stored under, not only the one in the request.
		if plan.Info.ID != repo && !p.allowed(plan.Info.ID) {
			return "", fmt.Errorf("%w: %s is named %s upstream, which pull-through is not allowed to fetch",
				mirror.ErrUpstreamNotFound, repo, plan.Info.ID)
		}
		c, err := plan.RecordLazy(ctx, p.reg)
		if err != nil {
			p.log.Warn("pull-through could not record the revision", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
			return "", err
		}
		p.log.Info("pull-through recorded a revision; its files are fetched on demand", "repo", c.Repo, "revision", rev,
			"sha", c.SHA, "files", len(plan.Entries), "bytes", plan.Bytes())
		return c.Repo, nil
	})
}

// ensureFile fetches the content of one pending file of revision c.
func (p *puller) ensureFile(ctx context.Context, c registry.Commit, f registry.File) error {
	repo, err := p.reg.GetRepo(ctx, c.Repo)
	if err != nil {
		return err
	}
	if repo.Upstream == "" {
		return errNotFromUpstream
	}
	if p.maxFileBytes > 0 && f.Size > p.maxFileBytes {
		return errFileTooLarge{repo: c.Repo, path: f.Path, size: f.Size, limit: p.maxFileBytes}
	}
	return p.share(ctx, c.Repo+"@"+c.SHA+":"+f.Path, func(ctx context.Context) error {
		started := p.now()
		if err := mirror.FetchFile(ctx, p.upstream(), p.reg, c, f); err != nil {
			p.log.Warn("pull-through could not fetch a file", "repo", c.Repo, "sha", c.SHA, "path", f.Path, "error", err)
			return err
		}
		p.log.Info("pull-through fetched a file", "repo", c.Repo, "sha", c.SHA, "path", f.Path, "bytes", f.Size,
			"took", p.now().Sub(started).Round(time.Millisecond))
		return nil
	})
}

// pullFailure says whose failure a pull-through job's error is, for the
// answer to the client. Only what the upstream caused is reported as the
// upstream's (502, with the reason). Anything else that went wrong while
// fetching is the registry's own (its disk, its database): 500, logged in
// full, and described to the client no further.
func pullFailure(err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, errPullBusy), errors.Is(err, errShuttingDown):
		return err
	case mirror.FromUpstream(err):
		return errUpstream{err}
	}
	return errInternal{err}
}

// resolve is Registry.Resolve with pull-through: a revision the registry
// does not hold is recorded from upstream first, when that is enabled and
// the repository is allowed.
//
// Only two kinds of repository are ever looked up upstream: one the
// registry does not hold at all, and one that itself came from the upstream
// (by mirroring or pull-through, so its upstream field is set). A
// repository created here by a push or an upload is never added to from
// upstream: whoever holds the same name on the hub must not be able to put
// revisions into it.
func (s *Server) resolve(ctx context.Context, repo, rev string) (registry.Commit, error) {
	c, err := s.reg.Resolve(ctx, repo, rev)
	if err == nil || s.puller == nil {
		return c, err
	}
	switch {
	case errors.Is(err, registry.ErrRepoNotFound):
		// Not held at all. The allow list decides, below.
	case errors.Is(err, registry.ErrRevisionNotFound):
		held, repoErr := s.reg.GetRepo(ctx, repo)
		if repoErr != nil || held.Upstream == "" {
			return c, err
		}
	default:
		return c, err
	}
	// Nothing is sent upstream that the registry itself would not accept
	// as a name.
	if registry.ValidateRepoName(repo) != nil || !validRevision(rev) || !s.puller.allowed(repo) {
		return c, err
	}
	stored, pullErr := s.puller.ensure(ctx, repo, rev)
	switch {
	case pullErr == nil:
		return s.reg.Resolve(ctx, stored, rev)
	case errors.Is(pullErr, mirror.ErrUpstreamNotFound), errors.Is(pullErr, registry.ErrInvalid),
		errors.Is(pullErr, registry.ErrLocalRepo):
		// Upstream does not have it either, or it belongs in a repository
		// that was created here: the local "not found" stands.
		return c, err
	default:
		return c, pullFailure(pullErr)
	}
}

// validRevision reports whether rev could name a revision: a full revision
// id, or a branch or tag name the registry would accept.
func validRevision(rev string) bool {
	return registry.IsSHA(rev) || registry.ValidateRefName(rev) == nil
}
