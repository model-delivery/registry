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
//     same thing joins it instead of starting another.
//   - Nothing is served unverified. A file is only handed to a client once
//     all of it is stored and has matched its digest.
//   - What the registry already holds is served as is. Pull-through fills
//     gaps; it never refreshes a branch that has moved upstream.
//     `modelctl mirror` does that, and also fetches a whole revision at
//     once, which is what guarantees it is available offline.

// errUpstream wraps a pull-through failure that is the upstream's or the
// network's doing, so it is reported as a gateway error, not as "not found".
type errUpstream struct{ err error }

func (e errUpstream) Error() string { return "could not fetch from upstream: " + e.err.Error() }
func (e errUpstream) Unwrap() error { return e.err }

const (
	// How long an upstream "not found" is remembered, so a client retrying
	// a wrong name does not turn into a stream of upstream requests.
	missTTL = time.Minute
	// How many fetches may run at once.
	maxConcurrentPulls = 4
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
	repo string // for a revision: the id it was stored under
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

// share runs work once per key however many callers ask for it at the same
// time, on the server's own context, and lets each caller wait only as
// long as its own ctx lives.
func (p *puller) share(ctx context.Context, key string, work func(ctx context.Context, job *pullJob)) (*pullJob, error) {
	p.mu.Lock()
	job := p.jobs[key]
	if job == nil {
		job = &pullJob{done: make(chan struct{})}
		p.jobs[key] = job
		// Deliberately not the request's context: the fetch must outlive the
		// request that started it (see the note at the top of this file).
		go func() { //nolint:gosec // G118: detached from the request on purpose.
			bg, cancel := context.WithTimeout(context.Background(), pullTimeout)
			defer cancel()
			defer func() {
				p.mu.Lock()
				delete(p.jobs, key)
				p.mu.Unlock()
				close(job.done)
			}()
			p.slots <- struct{}{}
			defer func() { <-p.slots }()
			work(bg, job)
		}()
	}
	p.mu.Unlock()

	select {
	case <-job.done:
		return job, job.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ensure records repo@rev from the upstream's listing if the registry does
// not hold it. It returns the id the revision is stored under, which is
// the upstream's canonical id and can differ from repo after a rename.
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
	p.mu.Unlock()

	job, err := p.share(ctx, key, func(ctx context.Context, job *pullJob) {
		plan, err := mirror.Prepare(ctx, p.upstream(), repo, rev)
		if err != nil {
			job.err = err
			if errors.Is(err, mirror.ErrUpstreamNotFound) {
				p.mu.Lock()
				if len(p.misses) > 10_000 {
					clear(p.misses)
				}
				p.misses[key] = time.Now().Add(missTTL)
				p.mu.Unlock()
				return
			}
			p.log.Warn("pull-through could not resolve upstream", "repo", repo, "revision", rev, "error", err)
			return
		}
		c, err := plan.RecordLazy(ctx, p.reg)
		if err != nil {
			job.err = err
			p.log.Warn("pull-through could not record the revision", "repo", plan.Info.ID, "sha", plan.Info.SHA, "error", err)
			return
		}
		job.repo = c.Repo
		p.log.Info("pull-through recorded a revision; its files are fetched on demand", "repo", c.Repo, "revision", rev,
			"sha", c.SHA, "files", len(plan.Entries), "bytes", plan.Bytes())
	})
	if err != nil {
		return "", err
	}
	return job.repo, nil
}

// ensureFile fetches the content of one pending file of revision c.
func (p *puller) ensureFile(ctx context.Context, c registry.Commit, f registry.File) error {
	_, err := p.share(ctx, c.Repo+"@"+c.SHA+":"+f.Path, func(ctx context.Context, job *pullJob) {
		started := time.Now()
		if err := mirror.FetchFile(ctx, p.upstream(), p.reg, c, f); err != nil {
			job.err = err
			p.log.Warn("pull-through could not fetch a file", "repo", c.Repo, "sha", c.SHA, "path", f.Path, "error", err)
			return
		}
		p.log.Info("pull-through fetched a file", "repo", c.Repo, "sha", c.SHA, "path", f.Path, "bytes", f.Size,
			"took", time.Since(started).Round(time.Millisecond))
	})
	return err
}

// resolve is Registry.Resolve with pull-through: a revision the registry
// does not hold is recorded from upstream first, when that is enabled and
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
