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
)

// These tests reach inside the pull-through coordinator, because filling
// its queue from outside would need dozens of slow upstream downloads.

func TestPullQueueIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	release := make(chan struct{})
	held := func(context.Context, *pullJob) { <-release }

	// Fill every slot and every place in the queue.
	const total = maxConcurrentPulls + maxWaitingPulls
	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.share(ctx, fmt.Sprintf("job-%d", i), held); err != nil {
				t.Errorf("job %d: %v", i, err)
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		jobs, waiting := len(p.jobs), p.waiting
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
	if _, err := p.share(ctx, "one-too-many", held); !errors.Is(err, errPullBusy) {
		t.Fatalf("got %v, want errPullBusy", err)
	}
	// A request for something already queued joins it, as before.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := p.share(short, "job-0", held); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("joining a queued job: got %v, want it to wait", err)
	}

	close(release)
	wg.Wait()
	// With the queue drained, new fetches are accepted again.
	if _, err := p.share(ctx, "afterwards", func(context.Context, *pullJob) {}); err != nil {
		t.Fatalf("after the queue drained: %v", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waiting != 0 {
		t.Fatalf("%d jobs still counted as waiting", p.waiting)
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
	if len(p.misses) != 0 || len(p.jobs) != 0 {
		t.Fatalf("kept %d misses and %d jobs for a name that cannot exist", len(p.misses), len(p.jobs))
	}
}
