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

	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// Pull-through caching: when a client asks for a revision the registry
// does not hold, fetch it from the upstream hub, store it, and then answer.
// It is the same verified, all-or-nothing mirror that `modelctl mirror`
// runs, started by a read instead of a command.
//
// Two properties matter:
//
//   - The fetch belongs to the server, not to the request that started it.
//     A large model takes minutes, and clients give up sooner than that. If
//     the client disconnects the fetch carries on, and the next request for
//     the same revision joins it instead of starting another.
//   - A revision the registry already holds is served as is. Pull-through
//     fills gaps; it never refreshes a branch that has moved upstream.
//     `modelctl mirror` does that.

// errUpstream wraps a pull-through failure that is the upstream's or the
// network's doing, so it is reported as a gateway error, not as "not found".
type errUpstream struct{ err error }

func (e errUpstream) Error() string { return "could not fetch from upstream: " + e.err.Error() }
func (e errUpstream) Unwrap() error { return e.err }

const (
	// How long an upstream "not found" is remembered, so a client retrying
	// a wrong name does not turn into a stream of upstream requests.
	missTTL = time.Minute
	// How many revisions may be fetched at once. Each fetches several files
	// in parallel already.
	maxConcurrentPulls = 2
	// The most time one fetch may take before it is abandoned.
	pullTimeout = 12 * time.Hour
)

type puller struct {
	reg      *registry.Registry
	upstream func() *mirror.Upstream
	allow    []string
	log      *slog.Logger

	mu     sync.Mutex
	jobs   map[string]*pullJob
	misses map[string]time.Time
	slots  chan struct{}
}

type pullJob struct {
	done chan struct{}
	repo string // the id the revision was stored under
	err  error
}

func newPuller(reg *registry.Registry, upstream func() *mirror.Upstream, allow []string, log *slog.Logger) *puller {
	return &puller{
		reg: reg, upstream: upstream, allow: allow, log: log,
		jobs: map[string]*pullJob{}, misses: map[string]time.Time{},
		slots: make(chan struct{}, maxConcurrentPulls),
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

// ensure makes repo@rev available, fetching it if no fetch is already under
// way. It returns the id the revision is stored under, which is the
// upstream's canonical id and can differ from repo after a rename there.
// It waits for the fetch only as long as ctx lives; the fetch itself is
// not tied to ctx.
func (p *puller) ensure(ctx context.Context, repo, rev string) (string, error) {
	key := repo + "@" + rev

	p.mu.Lock()
	if until, missed := p.misses[key]; missed {
		if time.Now().Before(until) {
			p.mu.Unlock()
			return "", fmt.Errorf("%w: %s@%s", mirror.ErrUpstreamNotFound, repo, rev)
		}
		delete(p.misses, key)
	}
	job := p.jobs[key]
	if job == nil {
		job = &pullJob{done: make(chan struct{})}
		p.jobs[key] = job
		// Deliberately not the request's context: the fetch must outlive the
		// request that started it (see the note at the top of this file).
		go p.run(key, repo, rev, job) //nolint:gosec // G118: detached from the request on purpose.
	}
	p.mu.Unlock()

	select {
	case <-job.done:
		return job.repo, job.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *puller) run(key, repo, rev string, job *pullJob) {
	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()
	defer func() {
		p.mu.Lock()
		delete(p.jobs, key)
		if errors.Is(job.err, mirror.ErrUpstreamNotFound) {
			if len(p.misses) > 10_000 {
				clear(p.misses)
			}
			p.misses[key] = time.Now().Add(missTTL)
		}
		p.mu.Unlock()
		close(job.done)
	}()

	p.slots <- struct{}{}
	defer func() { <-p.slots }()

	started := time.Now()
	plan, err := mirror.Prepare(ctx, p.upstream(), repo, rev)
	if err != nil {
		job.err = err
		if !errors.Is(err, mirror.ErrUpstreamNotFound) {
			p.log.Warn("pull-through could not resolve upstream", "repo", repo, "revision", rev, "error", err)
		}
		return
	}
	p.log.Info("pull-through started", "repo", plan.Info.ID, "revision", rev, "sha", plan.Info.SHA,
		"files", len(plan.Entries), "bytes", plan.Bytes())
	done, err := plan.Execute(ctx, p.reg, 4, nil)
	if err != nil {
		job.err = err
		p.log.Warn("pull-through failed", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
		return
	}
	job.repo = done.Repo
	p.log.Info("pull-through complete", "repo", done.Repo, "sha", done.SHA, "files", done.Files,
		"downloaded_bytes", done.DownloadedBytes, "reused_bytes", done.ReusedBytes,
		"took", time.Since(started).Round(time.Millisecond))
}

// resolve is Registry.Resolve with pull-through: a revision the registry
// does not hold is fetched from upstream first, when that is enabled and
// the repository is allowed.
func (s *Server) resolve(ctx context.Context, repo, rev string) (registry.Commit, error) {
	c, err := s.reg.Resolve(ctx, repo, rev)
	if err == nil || s.puller == nil {
		return c, err
	}
	if !errors.Is(err, registry.ErrRepoNotFound) && !errors.Is(err, registry.ErrRevisionNotFound) {
		return c, err
	}
	if registry.ValidateRepoName(repo) != nil || !s.puller.allowed(repo) {
		return c, err
	}
	stored, pullErr := s.puller.ensure(ctx, repo, rev)
	switch {
	case pullErr == nil:
		return s.reg.Resolve(ctx, stored, rev)
	case errors.Is(pullErr, mirror.ErrUpstreamNotFound), errors.Is(pullErr, registry.ErrInvalid):
		// Upstream does not have it either: the local "not found" stands.
		return c, err
	case errors.Is(pullErr, context.Canceled), errors.Is(pullErr, context.DeadlineExceeded):
		return c, pullErr
	default:
		return c, errUpstream{pullErr}
	}
}
