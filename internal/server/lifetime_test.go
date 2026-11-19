// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/server"
)

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{at: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// A signed upload URL is good for twelve hours and not a second longer.
func TestSignedUploadURLExpires(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	in := start(t, server.Config{AnonymousRead: true, Now: clock.Now})
	early, late := strings.Repeat("sent in time ", 300), strings.Repeat("sent too late ", 300)
	urls := batch(t, in, "acme/up", early, late)

	// Just inside the twelve hours: accepted.
	clock.Advance(12*time.Hour - time.Second)
	resp := do(t, http.MethodPut, urls[sha(early)], "", strings.NewReader(early), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload before the URL expired: status %d (%s)", resp.StatusCode, body(t, resp))
	}
	if _, ok, _ := in.reg.StatBlob(context.Background(), sha(early)); !ok {
		t.Fatal("the upload was not stored")
	}

	// Just past them: refused, with what to do, and nothing stored.
	clock.Advance(2 * time.Second)
	resp = do(t, http.MethodPut, urls[sha(late)], "", strings.NewReader(late), nil)
	msg := body(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(msg, "expired") || !strings.Contains(msg, "start the upload again") {
		t.Fatalf("upload after the URL expired: status %d: %s", resp.StatusCode, msg)
	}
	if _, ok, _ := in.reg.StatBlob(context.Background(), sha(late)); ok {
		t.Fatal("an upload to an expired URL was stored")
	}

	// A new URL from the batch route works again.
	fresh := batch(t, in, "acme/up", late)
	if resp := do(t, http.MethodPut, fresh[sha(late)], "", strings.NewReader(late), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload to a new URL: status %d", resp.StatusCode)
	}
}

// An upstream "not found" is remembered for a minute, so a client retrying
// a wrong name does not become a stream of upstream requests. After that
// the upstream is asked again: the repository may have been created.
func TestRememberedMissExpires(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	clock := newFakeClock()
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true, Now: clock.Now})

	ask := func(want int) {
		t.Helper()
		resp := do(t, http.MethodGet, local.url+"/api/models/acme/later/revision/main", "", nil, nil)
		if resp.StatusCode != want {
			t.Fatalf("status %d, want %d (%s)", resp.StatusCode, want, body(t, resp))
		}
	}
	ask(http.StatusNotFound)
	if got := hub.lookups.Load(); got != 1 {
		t.Fatalf("%d upstream lookups for the first request, want 1", got)
	}
	// Asked again, and again just before the minute is up: answered from memory.
	ask(http.StatusNotFound)
	clock.Advance(59 * time.Second)
	ask(http.StatusNotFound)
	if got := hub.lookups.Load(); got != 1 {
		t.Fatalf("%d upstream lookups inside the minute, want still 1", got)
	}
	// Meanwhile the repository appears upstream. Once the minute has passed
	// the registry asks again and finds it.
	push(t, hub.inst, "acme/later", map[string]string{"config.json": "{}"}, client.PushOptions{})
	ask(http.StatusNotFound)
	clock.Advance(2 * time.Second)
	ask(http.StatusOK)
	if got := hub.lookups.Load(); got != 2 {
		t.Fatalf("%d upstream lookups after the miss expired, want 2", got)
	}
}

// With every file slot taken by a download in progress, the first request
// for another repository still gets its listing: listings have slots of
// their own.
func TestListingCompletesWhileEveryFileSlotIsBusy(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	big := map[string]string{"a.bin": "aaaa", "b.bin": "bbbb", "c.bin": "cccc", "d.bin": "dddd"}
	push(t, hub.inst, "acme/big", big, client.PushOptions{})
	hub.hold = make(chan struct{})
	local := start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})

	// Four downloads, each of a different file, all held back upstream.
	var wg sync.WaitGroup
	for name := range big {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, local.url+"/acme/big/resolve/main/"+name, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d", name, resp.StatusCode)
			}
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for hub.files.Load() < int64(len(big)) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d downloads reached the upstream", hub.files.Load(), len(big))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Every file slot is now busy. A repository never seen before is listed
	// all the same, by each of the routes that list.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, path := range []string{"/api/models/acme/tiny/revision/main", "/api/models/acme/tiny/tree/main", "/api/v1/repos/acme/tiny/revisions/main"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.url+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: the listing waited behind the file downloads: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
	// So is the metadata of one of its files, which costs no slot either.
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, local.url+"/acme/tiny/resolve/main/model.safetensors", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD of a listed file: %v", err)
	}
	_ = resp.Body.Close()
	if got := hub.files.Load(); got != int64(len(big)) {
		t.Fatalf("upstream was asked for %d files, want only the %d held ones", got, len(big))
	}

	close(hub.hold)
	wg.Wait()
}

// Shutdown ends a fetch in flight and waits for it. The client that was
// waiting is told to come back, as is one that asks afterwards. A client
// that merely hangs up, by contrast, does not end the fetch: see
// TestPullThroughSurvivesTheClientLeaving.
func TestShutdownEndsFetchesInFlight(t *testing.T) {
	t.Parallel()
	hub := newCountingHub(t, server.Config{AnonymousRead: true})
	hub.hold = make(chan struct{})
	t.Cleanup(func() { close(hub.hold) })

	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.New(reg, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	waiting := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/acme/tiny/resolve/main/model.safetensors", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("the waiting client: %v", err)
			resp = nil
		}
		waiting <- resp
	}()
	deadline := time.Now().Add(10 * time.Second)
	for hub.files.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the fetch never reached the upstream")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown has returned, so the fetch is over: the registry can be
	// closed with nothing still writing to it.
	c, err := reg.Resolve(context.Background(), "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	if f, err := reg.File(context.Background(), c, "model.safetensors"); err != nil || !f.Pending {
		t.Fatalf("the abandoned file is recorded as stored: %+v, %v", f, err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}

	resp := <-waiting
	if resp == nil {
		t.FailNow()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the waiting client: status %d, Retry-After %q; want 503 and when to come back",
			resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// Shutdown is safe to call again.
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
