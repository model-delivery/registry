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

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/metrics"
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
//   - Nothing unverified is stored, and no client is handed a whole file
//     that was not verified. A stored file is served only after all of it
//     matched its digest. A file still being fetched is sent to the clients
//     waiting for it as it arrives, all but its end, which is held back
//     until the digest has been checked (stream.go has the rule and what
//     follows from it).
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
	repo, path  string // repo is the repository's key
	size, limit int64
}

func (e errFileTooLarge) Error() string {
	return fmt.Sprintf("%s is %s, more than this registry fetches on demand (REGISTRY_PULL_THROUGH_MAX_FILE_BYTES is %d): "+
		"raise that setting, or fetch the file ahead of time with `%s`", e.path, kit.HumanBytes(e.size), e.limit, mirrorCommand(e.repo))
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
	// How many files may be distrusted one by one (puller.distrust) before
	// all are. It bounds memory; an entry is one failed fetch.
	maxDistrusted = 10_000
)

type puller struct {
	reg      *registry.Registry
	upstream func() *mirror.Upstream
	allow    []string
	// maxFileBytes is the largest file fetched on demand; 0 means no limit.
	maxFileBytes int64
	// stream sends a file to the clients waiting for it while it is being
	// fetched (stream.go). Off, a file is only served once it is stored.
	stream bool
	log    *slog.Logger
	// ctx is the lifetime every job runs on: the server's, which ends at
	// Server.Shutdown. It is never a request's.
	ctx context.Context
	now func() time.Time
	// m counts what the coordinator does; its zero value counts nothing.
	m pullMetrics

	mu sync.Mutex
	// The jobs in progress, of each kind, by what they fetch. A file fetch
	// yields nothing but success or failure; a listing yields the id the
	// revision was stored under.
	fileJobs    map[string]*pullJob[struct{}]
	listingJobs map[string]*pullJob[string]
	running     int // jobs of either kind that have not finished
	waiting     int // of those, the ones that do not hold a slot yet
	misses      map[string]time.Time
	// distrusted is the files, by ETag, of which bytes were sent to a client
	// and then never verified; distrustAll stands in for the set once it is
	// full. See puller.distrust.
	distrusted  map[string]struct{}
	distrustAll bool
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
	// tail follows a file fetch's download, for the requests that are sent
	// the file while it arrives. It is nil for a listing and when streaming
	// is off.
	tail *blob.Tail
	// ended, when set, is called with the job's error once its work is
	// over, before anyone waiting on done is released.
	ended func(err error)
	// started and finished, when set, count the job: started when it is
	// taken on, which a request that joins an existing job does not do
	// again, and finished with how it ended.
	started  *metrics.Counter
	finished func(err error)
}

// newJob returns a job that has not started.
func newJob[T any]() *pullJob[T] { return &pullJob[T]{done: make(chan struct{})} }

// wait returns the job's result once it has finished, or ctx's error if ctx
// ends first; the job carries on either way.
func (j *pullJob[T]) wait(ctx context.Context) (T, error) {
	select {
	case <-j.done:
		return j.result, j.err
	case <-ctx.Done():
		var none T
		return none, ctx.Err()
	}
}

// newPuller builds a coordinator whose jobs run until they finish. The
// server gives it its own lifetime and clock (see New).
func newPuller(reg *registry.Registry, upstream func() *mirror.Upstream, allow []string, maxFileBytes int64, log *slog.Logger) *puller {
	return &puller{
		reg: reg, upstream: upstream, allow: allow, maxFileBytes: maxFileBytes, log: log,
		ctx: context.Background(), now: time.Now,
		fileJobs: map[string]*pullJob[struct{}]{}, listingJobs: map[string]*pullJob[string]{},
		misses: map[string]time.Time{}, distrusted: map[string]struct{}{},
		drained:  make(chan struct{}),
		slots:    make(chan struct{}, maxConcurrentPulls),
		listings: make(chan struct{}, maxConcurrentListings),
	}
}

// allowed reports whether the repository with key repo may be fetched on
// demand. With no patterns configured, every repository may, models and
// datasets. A pattern is a glob over the whole key, for example
// "meta-llama/*" or "*/*-GGUF". The key of a dataset begins with
// "datasets/" and a glob's * never matches a slash, so those patterns name
// models only, as they always have, and "datasets/meta-llama/*" or
// "datasets/*/*" name datasets.
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
	_, err := shareJob(ctx, p, p.fileJobs, p.slots, key, newJob[struct{}](), func(ctx context.Context) (struct{}, error) {
		return struct{}{}, work(ctx)
	})
	return err
}

// shareListing is share for a revision listing, which takes its slot from
// the listings' own pool and yields the id the revision is stored under.
func (p *puller) shareListing(ctx context.Context, key string, work func(ctx context.Context) (string, error)) (string, error) {
	fresh := newJob[string]()
	fresh.started = p.m.listingsStarted
	fresh.finished = func(err error) { p.m.listings.With(pullOutcome(err, true)).Inc() }
	return shareJob(ctx, p, p.listingJobs, p.listings, key, fresh, work)
}

// shareJob is what share and shareListing have in common: it joins the job
// for key among jobs, or starts fresh as that job, taking its slot from
// pool, and waits for the result as long as ctx lives.
func shareJob[T any](ctx context.Context, p *puller, jobs map[string]*pullJob[T], pool chan struct{}, key string, fresh *pullJob[T], work func(ctx context.Context) (T, error)) (T, error) {
	job, err := joinJob(p, jobs, pool, key, fresh, work)
	if err != nil {
		var none T
		return none, err
	}
	return job.wait(ctx)
}

// joinJob returns the job for key among jobs without waiting for it. If
// there is none it starts fresh as that job, running work with a slot from
// pool; otherwise fresh and work are not used.
func joinJob[T any](p *puller, jobs map[string]*pullJob[T], pool chan struct{}, key string, fresh *pullJob[T], work func(ctx context.Context) (T, error)) (*pullJob[T], error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if job := jobs[key]; job != nil {
		return job, nil
	}
	switch {
	case p.closed:
		return nil, errShuttingDown
	case p.waiting >= maxWaitingPulls:
		p.m.queueFull.Inc()
		return nil, errPullBusy
	}
	p.waiting++
	p.running++
	job := fresh
	jobs[key] = job
	job.started.Inc()
	// Deliberately not the request's context: the fetch must outlive the
	// request that started it (see the note at the top of this file).
	go func() {
		job.err = p.do(pool, func(ctx context.Context) (err error) {
			job.result, err = work(ctx)
			return err
		})
		if job.ended != nil {
			job.ended(job.err)
		}
		if job.finished != nil {
			job.finished(job.err)
		}
		p.finish(func() {
			delete(jobs, key)
			close(job.done)
		})
	}()
	return job, nil
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
			return c.Key(), nil
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
		if plan.Repo != repo && !p.allowed(plan.Repo) {
			return "", fmt.Errorf("%w: %s is named %s upstream, which pull-through is not allowed to fetch",
				mirror.ErrUpstreamNotFound, repo, plan.Repo)
		}
		c, err := plan.RecordLazy(ctx, p.reg)
		if err != nil {
			p.log.Warn("pull-through could not record the revision", "repo", plan.Repo, "sha", plan.Info.SHA, "error", err)
			return "", err
		}
		p.m.recorded.Inc()
		p.log.Info("pull-through recorded a revision; its files are fetched on demand", "repo", c.Key(), "revision", rev,
			"sha", c.SHA, "files", len(plan.Entries), "bytes", plan.Bytes())
		return c.Key(), nil
	})
}

// ensureFile fetches the content of one pending file of revision c, and
// returns once it is stored or the fetch has failed.
func (p *puller) ensureFile(ctx context.Context, c registry.Commit, f registry.File) error {
	job, err := p.fetchFile(ctx, c, f)
	if err != nil {
		return err
	}
	_, err = job.wait(ctx)
	return err
}

// fetchFile starts the fetch of one pending file of revision c, or joins
// the one in progress, and returns its job without waiting for it. With
// streaming on, the job has a tail that requests can follow.
func (p *puller) fetchFile(ctx context.Context, c registry.Commit, f registry.File) (*pullJob[struct{}], error) {
	repo, err := p.reg.GetRepo(ctx, c.Key())
	if err != nil {
		return nil, err
	}
	if repo.Upstream == "" {
		return nil, errNotFromUpstream
	}
	if p.maxFileBytes > 0 && f.Size > p.maxFileBytes {
		return nil, errFileTooLarge{repo: c.Key(), path: f.Path, size: f.Size, limit: p.maxFileBytes}
	}
	job := newJob[struct{}]()
	job.started = p.m.fetchesStarted
	job.finished = func(err error) { p.m.fetches.With(pullOutcome(err, false)).Inc() }
	if p.stream {
		job.tail = blob.NewTail()
		job.ended = func(err error) {
			// The tail ends first, so no unverified byte is handed out after
			// this; then the file is distrusted, before anyone waiting on the
			// job, or a request that comes after it, is answered.
			if job.tail.End() && err != nil {
				p.distrust(c, f, err)
			}
		}
	}
	return joinJob(p, p.fileJobs, p.slots, c.Key()+"@"+c.SHA+":"+f.Path, job, func(ctx context.Context) (struct{}, error) {
		started := p.now()
		downloaded, err := mirror.FetchFileFollowed(ctx, p.upstream(), p.reg, c, f, job.tail)
		if err != nil {
			p.log.Warn("pull-through could not fetch a file", "repo", c.Key(), "sha", c.SHA, "path", f.Path, "error", err)
			return struct{}{}, err
		}
		if downloaded {
			p.m.fetchedBytes.Add(f.Size)
		}
		p.log.Info("pull-through fetched a file", "repo", c.Key(), "sha", c.SHA, "path", f.Path, "bytes", f.Size,
			"took", p.now().Sub(started).Round(time.Millisecond))
		return struct{}{}, nil
	})
}

// distrust records that bytes of f were sent to a client while it was being
// fetched and were then never verified, because the fetch failed with err.
// Some client may be holding those bytes as the start of the file, and the
// registry cannot tell which. So from here until the server stops, f's
// content (named by its ETag, wherever it is listed) is treated two ways
// differently (stream.go):
//
//   - it is not sent while being fetched again: the next fetch is verified
//     and stored first, and a failure is answered with a status;
//   - a request for part of it is answered with all of it, so that a client
//     resuming a download replaces what it has and does not add to it.
//
// The set is bounded. If it ever fills, every file is treated that way,
// which costs resumed downloads their saving and never their correctness.
func (p *puller) distrust(c registry.Commit, f registry.File, err error) {
	p.mu.Lock()
	full := len(p.distrusted) >= maxDistrusted
	if full {
		p.distrustAll = true
	} else {
		p.distrusted[f.ETag()] = struct{}{}
	}
	p.mu.Unlock()
	p.log.Warn("pull-through sent part of a file to clients before its fetch failed; until the registry restarts this file is verified before it is sent, and a request for part of it is answered with the whole",
		"repo", c.Key(), "sha", c.SHA, "path", f.Path, "error", err)
	if full {
		p.log.Error("so many streamed fetches have failed that the registry now answers every request for part of a file with the whole file, until it restarts", "limit", maxDistrusted)
	}
}

// distrusts reports whether bytes of the file with this ETag were sent
// unverified and never vouched for (see distrust).
func (p *puller) distrusts(etag string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, listed := p.distrusted[etag]
	return listed || p.distrustAll
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
	if registry.ValidateRepoKey(repo) != nil || !validRevision(rev) || !s.puller.allowed(repo) {
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
