// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// Every error a handler can be given has one status, and the ones Hugging
// Face clients act on have a code.
func TestStatusFor(t *testing.T) {
	t.Parallel()
	wrapped := func(err error) error { return fmt.Errorf("while doing something: %w", err) }
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unknown repository", wrapped(registry.ErrRepoNotFound), 404, "RepoNotFound"},
		{"unknown revision", wrapped(registry.ErrRevisionNotFound), 404, "RevisionNotFound"},
		{"unknown file", wrapped(registry.ErrEntryNotFound), 404, "EntryNotFound"},
		{"blob not stored", wrapped(blob.ErrNotFound), 404, "EntryNotFound"},
		{"unknown token", registry.ErrTokenNotFound, 404, ""},
		{"invalid input", wrapped(registry.ErrInvalid), 400, ""},
		{"blob not uploaded", registry.ErrBlobMissing, 400, ""},
		{"upload does not match its digest", wrapped(blob.ErrMismatch), 400, ""},
		{"conflict", registry.ErrConflict, 409, ""},
		{"not found upstream", wrapped(mirror.ErrUpstreamNotFound), 404, ""},
		{"upstream denied access", mirror.ErrUpstreamAuth, 502, ""},
		{"upstream pointed somewhere unsafe", mirror.ErrUnsafeUpstream, 502, ""},
		{"over a size limit", wrapped(errTooLarge{"too large"}), 413, ""},
		{"too many fetches waiting", errPullBusy, 503, ""},
		{"the server is stopping", errShuttingDown, 503, ""},
		{"the client went away", wrapped(context.Canceled), statusClientClosedRequest, ""},
		{"anything else", errors.New("disk on fire"), 500, ""},
		{"a deadline", context.DeadlineExceeded, 500, ""},

		// A pull-through failure is the upstream's or the registry's own,
		// and the wrapper decides, not what is inside it.
		{"upstream failure during pull-through", errUpstream{errors.New("connection reset")}, 502, ""},
		{"upstream sent the wrong bytes", errUpstream{wrapped(blob.ErrMismatch)}, 502, ""},
		{"upstream lost the file", errUpstream{mirror.ErrUpstreamNotFound}, 502, ""},
		{"local failure during pull-through", errInternal{errors.New("no space left on device")}, 500, ""},
		{"local failure that looks like bad input", errInternal{wrapped(registry.ErrInvalid)}, 500, ""},
		{"local failure that looks like not found", errInternal{registry.ErrEntryNotFound}, 500, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code := statusFor(tc.err)
			if status != tc.status || code != tc.code {
				t.Fatalf("statusFor(%v) = %d %q, want %d %q", tc.err, status, code, tc.status, tc.code)
			}
		})
	}
	if statusClientClosedRequest != 499 {
		t.Fatalf("the status for a cancelled request is %d; proxies and log readers know it as 499", statusClientClosedRequest)
	}
}

// pullFailure decides what a failed fetch is reported as.
func TestPullFailureIsTheUpstreamsOnlyWhenTheUpstreamCausedIt(t *testing.T) {
	t.Parallel()
	local := errors.New("create staging file: open /srv/registry/tmp/stage-1: no space left on device")
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"the registry's disk", local, 500},
		{"the registry's database, even with a familiar error inside", fmt.Errorf("record file: %w", registry.ErrEntryNotFound), 500},
		{"upstream not found", mirror.ErrUpstreamNotFound, 502},
		{"upstream denied access", mirror.ErrUpstreamAuth, 502},
		{"the request was cancelled", context.Canceled, statusClientClosedRequest},
		{"too many fetches waiting", errPullBusy, 503},
		{"the server is stopping", errShuttingDown, 503},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if status, _ := statusFor(pullFailure(tc.err)); status != tc.status {
				t.Fatalf("answered %d, want %d", status, tc.status)
			}
		})
	}

	// And what the client is shown of a local failure: nothing of it.
	logs := &strings.Builder{}
	s := &Server{log: slog.New(slog.NewTextHandler(logs, nil))}
	for name, write := range map[string]func(http.ResponseWriter, *http.Request, error){"registry API": s.fail, "Hugging Face": s.hfFail} {
		rec := httptest.NewRecorder()
		write(rec, httptest.NewRequest(http.MethodGet, "/acme/tiny/resolve/main/model.bin", nil), pullFailure(local))
		shown := rec.Body.String() + rec.Header().Get("X-Error-Message")
		if rec.Code != http.StatusInternalServerError || strings.Contains(shown, "/srv/registry") || strings.Contains(shown, "upstream") {
			t.Errorf("%s: status %d, shown %q", name, rec.Code, shown)
		}
	}
	if got := logs.String(); !strings.Contains(got, "/srv/registry/tmp/stage-1") || !strings.Contains(got, "level=ERROR") {
		t.Errorf("the local failure was not logged in full:\n%s", got)
	}
}

// How the client reached the server decides the links it is sent and
// whether its sign-in cookie is marked Secure.
func TestSchemeOfARequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tls       bool
		forwarded string
		https     bool
	}{
		{"plain HTTP", false, "", false},
		{"TLS terminated here", true, "", true},
		{"TLS terminated by a proxy", false, "https", true},
		{"a proxy that spells it differently", false, "HTTPS", true},
		{"a proxy forwarding plain HTTP", false, "http", false},
		{"a header that is not a scheme", false, "yes", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "http://registry.example:8080/", nil)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			want := "http://registry.example:8080"
			if tc.https {
				want = "https://registry.example:8080"
			}
			if got := baseURL(r); got != want {
				t.Errorf("baseURL = %q, want %q", got, want)
			}
			c := sessionCookie(r, "secret", sessionLifetime)
			if c.Secure != tc.https {
				t.Errorf("cookie Secure = %v, want %v", c.Secure, tc.https)
			}
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 30*24*3600 {
				t.Errorf("cookie = %+v, want HttpOnly, SameSite=Strict, thirty days", c)
			}
			if gone := sessionCookie(r, "", 0); gone.MaxAge >= 0 || gone.Secure != tc.https {
				t.Errorf("sign-out cookie = %+v, want one that deletes itself", gone)
			}
		})
	}
}

// The answer to a deletion or a garbage collection that could not remove
// every file is a success with a warning, never a path from the server.
func TestAnswerFreedWarnsWithoutTheReason(t *testing.T) {
	t.Parallel()
	logs := &strings.Builder{}
	s := &Server{log: slog.New(slog.NewTextHandler(logs, nil))}
	res := registry.GCResult{Blobs: 3, Bytes: 4096}

	rec := httptest.NewRecorder()
	s.answerFreed(rec, res, nil, gcWarning)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"blobs":3,"bytes":4096}` {
		t.Fatalf("a clean run: status %d body %s; want the result alone, as before", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	failure := fmt.Errorf("%w: remove blob abc: unlink /srv/registry/blobs/sha256/ab/abc: permission denied", registry.ErrBlobsNotFreed)
	s.answerFreed(rec, res, failure, gcWarning)
	var out struct {
		Blobs   int    `json:"blobs"`
		Bytes   int64  `json:"bytes"`
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || out.Blobs != 3 || out.Bytes != 4096 || out.Warning != gcWarning {
		t.Fatalf("status %d, %+v; want the result with the warning", rec.Code, out)
	}
	if strings.Contains(rec.Body.String(), "/srv/registry") {
		t.Fatalf("the answer carries a server path: %s", rec.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, "permission denied") || !strings.Contains(got, "level=WARN") {
		t.Fatalf("the reason was not logged:\n%s", got)
	}
}

// waitFor polls until ok, which looks at the coordinator under its lock.
func waitFor(t *testing.T, p *puller, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		done := ok()
		p.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fillFileSlots starts as many held file fetches as may run at once.
func fillFileSlots(t *testing.T, p *puller) (release func()) {
	t.Helper()
	hold := make(chan struct{})
	var wg sync.WaitGroup
	for i := range maxConcurrentPulls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.share(context.Background(), fmt.Sprintf("file-%d", i), func(context.Context) error { <-hold; return nil })
		}()
	}
	waitFor(t, p, "every file slot taken", func() bool { return len(p.slots) == maxConcurrentPulls && p.waiting == 0 })
	return func() { close(hold); wg.Wait() }
}

// A revision listing is one small request. It must not wait for a file
// fetch, which can run for hours, to give up its slot.
func TestListingsDoNotWaitBehindFileFetches(t *testing.T) {
	t.Parallel()
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	release := fillFileSlots(t, p)
	defer release()

	// A fifth file fetch waits.
	fifth := make(chan error, 1)
	go func() {
		err := p.share(context.Background(), "file-waiting", func(context.Context) error { return nil })
		fifth <- err
	}()
	waitFor(t, p, "the fifth fetch queued", func() bool { return p.waiting == 1 })

	// A listing does not.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ran := false
	if _, err := p.shareListing(ctx, "acme/other@main", func(context.Context) (string, error) { ran = true; return "", nil }); err != nil || !ran {
		t.Fatalf("a listing with every file slot busy: ran %v, %v", ran, err)
	}
	select {
	case err := <-fifth:
		t.Fatalf("the fifth file fetch ran while four were in flight (%v)", err)
	default:
	}
}

// The time a job is allowed starts when it gets its slot. Time spent
// queued behind other jobs is not taken out of it.
func TestJobTimeoutStartsWhenTheJobDoes(t *testing.T) {
	t.Parallel()
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	release := fillFileSlots(t, p)

	const queuedFor = 400 * time.Millisecond
	var allowed time.Duration
	finished := make(chan error, 1)
	go func() {
		err := p.share(context.Background(), "queued", func(ctx context.Context) error {
			deadline, _ := ctx.Deadline()
			allowed = time.Until(deadline)
			return nil
		})
		finished <- err
	}()
	waitFor(t, p, "the job queued", func() bool { return p.waiting == 1 })
	time.Sleep(queuedFor)
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if short := pullTimeout - allowed; short > queuedFor/2 {
		t.Fatalf("the job was allowed %s less than the full time, after waiting %s for a slot", short, queuedFor)
	}
}

// Stopping the coordinator cancels what is running, waits for it, and
// refuses what comes after.
func TestPullerStopsItsJobs(t *testing.T) {
	t.Parallel()
	lifetime, stop := context.WithCancel(context.Background())
	p := newPuller(nil, nil, nil, 0, slog.New(slog.DiscardHandler))
	p.ctx = lifetime

	// One job running, one queued behind full slots.
	results := make(chan error, maxConcurrentPulls+1)
	for i := range maxConcurrentPulls + 1 {
		go func() {
			err := p.share(context.Background(), fmt.Sprintf("job-%d", i), func(ctx context.Context) error {
				<-ctx.Done()
				time.Sleep(50 * time.Millisecond) // slow to notice
				return fmt.Errorf("write blob: %w", ctx.Err())
			})
			results <- err
		}()
	}
	waitFor(t, p, "four running and one queued", func() bool { return p.running == maxConcurrentPulls+1 && p.waiting == 1 })

	// With nothing cancelled, wait gives up at its deadline and says so.
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait with jobs still running: %v", err)
	}

	stop()
	if err := p.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	jobs, waiting := p.running, p.waiting
	p.mu.Unlock()
	if jobs != 0 || waiting != 0 || len(p.slots) != 0 {
		t.Fatalf("after wait returned: %d jobs, %d waiting, %d slots held", jobs, waiting, len(p.slots))
	}
	for range maxConcurrentPulls + 1 {
		if err := <-results; !errors.Is(err, errShuttingDown) {
			t.Errorf("a caller waiting on a stopped job got %v, want errShuttingDown", err)
		}
	}
	// Nothing starts afterwards.
	started := false
	if err := p.share(context.Background(), "late", func(context.Context) error { started = true; return nil }); !errors.Is(err, errShuttingDown) || started {
		t.Fatalf("a job after the stop: started %v, %v", started, err)
	}
	if _, err := p.shareListing(context.Background(), "late@main", func(context.Context) (string, error) { started = true; return "", nil }); !errors.Is(err, errShuttingDown) || started {
		t.Fatalf("a listing after the stop: started %v, %v", started, err)
	}
}

// A request for a blob's existence that fails has no body to say why, so
// the log must.
func TestStatBlobLogsWhenItFails(t *testing.T) {
	t.Parallel()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	s := &Server{reg: reg, log: slog.New(slog.NewTextHandler(logs, nil))}
	_ = reg.Close() // every query fails from here on

	digest := strings.Repeat("a", 64)
	req := httptest.NewRequest(http.MethodHead, "/api/v1/blobs/"+digest, nil)
	req.SetPathValue("sha256", digest)
	rec := httptest.NewRecorder()
	s.apiStatBlob(rec, req)
	if body, _ := io.ReadAll(rec.Body); rec.Code != http.StatusInternalServerError || len(body) != 0 {
		t.Fatalf("status %d body %q, want a bare 500", rec.Code, body)
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "/api/v1/blobs/"+digest) {
		t.Fatalf("a 500 with nothing in the log:\n%s", got)
	}
}
