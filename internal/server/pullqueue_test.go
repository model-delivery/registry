// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// These tests reach inside the pull-through coordinator, because filling
// its queue from outside would need dozens of slow upstream downloads.

func TestPullQueueIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	release := make(chan struct{})
	held := func(context.Context) error { <-release; return nil }

	// Fill every slot and every place in the queue.
	const total = maxConcurrentPulls + maxWaitingPulls
	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.share(ctx, fmt.Sprintf("job-%d", i), held); err != nil {
				t.Errorf("job %d: %v", i, err)
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		jobs, waiting := p.running, p.waiting
		p.mu.Unlock()
		if jobs == total && waiting == maxWaitingPulls {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never filled: %d jobs, %d waiting", jobs, waiting)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// One more distinct fetch is refused at once, without waiting.
	if err := p.share(ctx, "one-too-many", held); !errors.Is(err, errPullBusy) {
		t.Fatalf("got %v, want errPullBusy", err)
	}
	// A request for something already queued joins it, as before.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := p.share(short, "job-0", held); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joining a queued job: got %v, want it to wait", err)
	}

	close(release)
	wg.Wait()
	// With the queue drained, new fetches are accepted again.
	if err := p.share(ctx, "afterwards", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("after the queue drained: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting != 0 {
		t.Fatalf("%d jobs still counted as waiting", p.waiting)
	}
}

// The files distrusted after a failed streamed fetch are remembered one by
// one up to a bound. Past it nothing is forgotten: every file is
// distrusted instead, which costs resumed downloads and never correctness.
func TestDistrustIsBoundedAndFailsSafe(t *testing.T) {
	t.Parallel()
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	failed := errors.New("blob verification failed")
	lfs := func(i int) registry.File {
		return registry.File{Path: "weights.bin", LFS: true, SHA256: fmt.Sprintf("%064d", i)}
	}

	p.distrust(registry.Commit{Repo: "acme/tiny"}, lfs(0), failed)
	if !p.distrusts(lfs(0).ETag()) || p.distrusts(lfs(1).ETag()) {
		t.Fatal("one failed file: it should be distrusted, and no other")
	}
	for i := 1; i <= maxDistrusted; i++ {
		p.distrust(registry.Commit{Repo: "acme/tiny"}, lfs(i), failed)
	}
	p.mu.Lock()
	kept := len(p.distrusted)
	p.mu.Unlock()
	if kept > maxDistrusted {
		t.Fatalf("%d files remembered, more than the bound of %d", kept, maxDistrusted)
	}
	for _, i := range []int{0, maxDistrusted, maxDistrusted + 500} {
		if !p.distrusts(lfs(i).ETag()) {
			t.Fatalf("with the set full, file %d is trusted", i)
		}
	}
}

func TestBusyIsAnswered503WithRetryAfter(t *testing.T) {
	t.Parallel()
	s := &Server{log: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	s.fail(rec, httptest.NewRequest(http.MethodGet, "/acme/tiny/resolve/main/config.json", nil), errPullBusy)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if !strings.Contains(rec.Body.String(), "busy fetching") {
		t.Fatalf("body does not say why: %s", rec.Body.String())
	}
}

func TestOverlongNamesAreNeitherLookedUpNorRemembered(t *testing.T) {
	t.Parallel()
	p := newPuller(nil, func() *mirror.Upstream {
		t.Error("the upstream was contacted")
		return &mirror.Upstream{}
	}, nil, 0, slog.New(slog.DiscardHandler))
	_, err := p.ensure(context.Background(), "acme/tiny", strings.Repeat("x", maxPullKey))
	if !errors.Is(err, mirror.ErrUpstreamNotFound) {
		t.Fatalf("got %v, want ErrUpstreamNotFound", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.misses) != 0 || p.running != 0 {
		t.Fatalf("kept %d misses and %d jobs for a name that cannot exist", len(p.misses), p.running)
	}
}
