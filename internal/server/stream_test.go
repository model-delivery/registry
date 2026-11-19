// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// Serve while fetching: a file pull-through is still downloading is sent to
// the clients waiting for it, all but its end, which waits for the digest.
//
// These tests need a file larger than what is held back (a mebibyte) and
// an upstream that sends it only as far as the test says, so that "the
// client has these bytes while the fetch is still running" is something a
// test can state without sleeping.

const (
	mib = 1 << 20
	// heldBack is the server's streamHoldBack: how much of a file's end is
	// not sent before the file is verified.
	heldBack = mib
	// bigSize is the size of the file these tests fetch, and sendable how
	// much of it may reach a client before it is verified.
	bigSize  = 3 * mib
	sendable = bigSize - heldBack
	// patience bounds every wait. It is reached only when something that
	// should have happened did not.
	patience = 15 * time.Second
)

// bigFile is the content of acme/big's weights.bin: numbered lines, so any
// two stretches of it differ and a misplaced byte shows.
var bigFile = func() string {
	var b strings.Builder
	for i := 0; b.Len() < bigSize; i++ {
		fmt.Fprintf(&b, "%015d\n", i)
	}
	return b.String()[:bigSize]
}()

// tamperedFile is bigFile with bytes changed near the start: the same
// size, another digest.
var tamperedFile = bigFile[:10] + "TAMPERED" + bigFile[18:]

const (
	bigRepo = "acme/big"
	bigPath = "/acme/big/resolve/main/weights.bin"
)

// pacedHub is an upstream hub holding acme/big, which sends weights.bin
// only as far as it has been told to.
type pacedHub struct {
	url    string
	pushed client.PushResult
	// fetches counts the requests for weights.bin that reached the upstream.
	fetches atomic.Int64

	mu      sync.Mutex
	allowed int           // how many bytes of weights.bin may be sent
	moved   chan struct{} // closed when allowed changes
	body    string        // what is sent as weights.bin
	refuse  int           // when not 0, the status every request for it gets
	// cutAt, when not 0, breaks the connection of a download from the start
	// after that many bytes, and refuses the attempt to resume it.
	cutAt int
}

func newPacedHub(t *testing.T) *pacedHub {
	t.Helper()
	inst := regtest.Start(t, server.Config{AnonymousRead: true})
	h := &pacedHub{moved: make(chan struct{}), body: bigFile}
	h.pushed = push(t, inst, bigRepo, map[string]string{"config.json": `{"model_type":"big"}`, "weights.bin": bigFile}, client.PushOptions{})
	target, err := url.Parse(inst.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/resolve/") || !strings.HasSuffix(r.URL.Path, "/weights.bin") {
			proxy.ServeHTTP(w, r)
			return
		}
		h.fetches.Add(1)
		h.mu.Lock()
		body, refuse, cutAt := h.body, h.refuse, h.cutAt
		h.mu.Unlock()
		if refuse != 0 || (cutAt != 0 && r.Header.Get("Range") != "") {
			w.WriteHeader(max(refuse, http.StatusForbidden))
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		sent := 0
		for sent < len(body) {
			h.mu.Lock()
			upTo, moved := min(h.allowed, len(body)), h.moved
			h.mu.Unlock()
			if cutAt != 0 && sent >= cutAt {
				panic(http.ErrAbortHandler)
			}
			if upTo > sent {
				if _, err := io.WriteString(w, body[sent:upTo]); err != nil {
					return
				}
				_ = http.NewResponseController(w).Flush()
				sent = upTo
				continue
			}
			select {
			case <-moved:
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(front.Close)
	h.url = front.URL
	return h
}

// set changes how the hub answers, under its lock.
func (h *pacedHub) set(change func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	change()
}

// release lets the upstream send weights.bin as far as byte n.
func (h *pacedHub) release(n int) {
	h.set(func() {
		h.allowed = n
		close(h.moved)
		h.moved = make(chan struct{})
	})
}

// within fails the test if work has not returned in time, with what it was
// waiting for.
func within(t *testing.T, what string, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		work()
	}()
	select {
	case <-done:
	case <-time.After(patience):
		t.Fatalf("never happened: %s", what)
	}
}

// until polls for ok.
func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// get starts a download and returns once its headers have arrived.
func get(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	var resp *http.Response
	within(t, "an answer to GET "+url, func() { resp = do(t, http.MethodGet, url, "", nil, header) })
	return resp
}

// take reads exactly n bytes of a response's body.
func take(t *testing.T, resp *http.Response, n int) string {
	t.Helper()
	buf := make([]byte, n)
	var err error
	within(t, fmt.Sprintf("%d more bytes of the body", n), func() { _, err = io.ReadFull(resp.Body, buf) })
	if err != nil {
		t.Fatalf("reading %d bytes: %v", n, err)
	}
	return string(buf)
}

// rest reads a response's body to its end, and returns the error it ended
// with, if it did not end cleanly.
func rest(t *testing.T, resp *http.Response) (string, error) {
	t.Helper()
	var got []byte
	var err error
	within(t, "the end of the body", func() { got, err = io.ReadAll(resp.Body) })
	return string(got), err
}

// stored reports whether weights.bin of acme/big has been fetched and
// recorded.
func stored(t *testing.T, reg *registry.Registry) bool {
	t.Helper()
	c, err := reg.Resolve(context.Background(), bigRepo, "main")
	if err != nil {
		return false
	}
	f, err := reg.File(context.Background(), c, "weights.bin")
	return err == nil && !f.Pending
}

// nothingKept fails the test if anything of weights.bin, genuine or
// tampered, is stored, recorded or left in the staging directory.
func nothingKept(t *testing.T, in *regtest.Instance) {
	t.Helper()
	ctx := context.Background()
	if stored(t, in.Reg) {
		t.Fatal("the file is recorded as stored")
	}
	for name, content := range map[string]string{"tampered": tamperedFile, "genuine": bigFile} {
		if _, ok, _ := in.Reg.StatBlob(ctx, kit.SHA256Hex(content)); ok {
			t.Fatalf("a blob is recorded under the digest of the %s content", name)
		}
		if rc, _, err := in.Reg.OpenBlob(ctx, kit.SHA256Hex(content)); err == nil {
			_ = rc.Close()
			t.Fatalf("the %s content is in the store", name)
		}
	}
	// Staging files go when the last response reading them has ended.
	until(t, "the staging directory emptied", func() bool {
		entries, err := os.ReadDir(filepath.Join(in.Data, "tmp"))
		return err == nil && len(entries) == 0
	})
}

func streaming(hub *pacedHub) server.Config {
	return server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true}
}

// Several clients are sent one fetch as it arrives: the one that started
// it, one that joins midway, and one that resumes from an offset. Each gets
// its bytes before the fetch has finished, and all of them right.
func TestStreamSendsOneFetchToEveryClientWhileItArrives(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	local := regtest.Start(t, streaming(hub))
	fileURL := local.URL + bigPath
	const half = bigSize / 2
	hub.release(half)

	// The upstream has sent half and is holding the rest. The first client
	// already has its answer and the first mebibyte.
	first := get(t, fileURL, nil)
	if first.StatusCode != http.StatusOK || first.ContentLength != bigSize {
		t.Fatalf("first client: status %d, length %d", first.StatusCode, first.ContentLength)
	}
	if got := take(t, first, mib); got != bigFile[:mib] {
		t.Fatal("the first client was sent other bytes than the file's first mebibyte")
	}
	// One that arrives now joins the same fetch and starts at the beginning.
	joiner := get(t, fileURL, nil)
	if got := take(t, joiner, mib); joiner.StatusCode != http.StatusOK || got != bigFile[:mib] {
		t.Fatalf("a client joining midway: status %d", joiner.StatusCode)
	}
	// One that already has the first million bytes asks for the rest.
	const have = 1_000_000
	resumed := get(t, fileURL, map[string]string{"Range": fmt.Sprintf("bytes=%d-", have)})
	if resumed.StatusCode != http.StatusPartialContent || resumed.ContentLength != bigSize-have ||
		resumed.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", have, bigSize-1, bigSize) {
		t.Fatalf("a ranged request during the fetch: status %d, length %d, Content-Range %q",
			resumed.StatusCode, resumed.ContentLength, resumed.Header.Get("Content-Range"))
	}
	if got := take(t, resumed, 1000); got != bigFile[have:have+1000] {
		t.Fatal("the ranged request was sent bytes from the wrong place")
	}
	if stored(t, local.Reg) {
		t.Fatal("the file is recorded as stored while the upstream still holds half of it")
	}

	// A crowd arrives; then the upstream sends the rest.
	const crowd = 6
	bodies := make(chan string, crowd)
	for range crowd {
		go func() {
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fileURL, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				bodies <- "request failed: " + err.Error()
				return
			}
			defer func() { _ = resp.Body.Close() }()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				bodies <- "body cut short: " + err.Error()
				return
			}
			bodies <- string(got)
		}()
	}
	hub.release(bigSize)
	for name, c := range map[string]struct {
		resp *http.Response
		want string
	}{
		"the first client":  {first, bigFile[mib:]},
		"the joiner":        {joiner, bigFile[mib:]},
		"the ranged client": {resumed, bigFile[have+1000:]},
	} {
		if got, err := rest(t, c.resp); err != nil || got != c.want {
			t.Errorf("%s: the rest of the file was %d bytes, want %d (%v)", name, len(got), len(c.want), err)
		}
	}
	for range crowd {
		within(t, "a download from the crowd", func() {
			if got := <-bodies; got != bigFile {
				t.Errorf("a client in the crowd got %d bytes: %.60s", len(got), got)
			}
		})
	}
	if got := hub.fetches.Load(); got != 1 {
		t.Fatalf("the upstream was asked for the file %d times, want once", got)
	}

	// Stored now, and served from the store with the same headers the
	// streamed answer had: the client decides what to do from them.
	until(t, "the file recorded as stored", func() bool { return stored(t, local.Reg) })
	again := get(t, fileURL, nil)
	if got, err := rest(t, again); err != nil || got != bigFile {
		t.Fatalf("from the store: %d bytes, %v", len(got), err)
	}
	names := func(h http.Header) []string {
		var out []string
		for name := range h {
			if name != "Date" {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	if a, b := names(first.Header), names(again.Header); !slices.Equal(a, b) {
		t.Fatalf("headers differ:\n streamed %v\n stored   %v", a, b)
	}
	for _, name := range names(again.Header) {
		if a, b := first.Header.Get(name), again.Header.Get(name); a != b {
			t.Errorf("%s: streamed %q, stored %q", name, a, b)
		}
	}
	for name, want := range map[string]string{
		"ETag": `"` + kit.SHA256Hex(bigFile) + `"`, "X-Repo-Commit": hub.pushed.SHA, "Accept-Ranges": "bytes",
		"Content-Type": "application/octet-stream", "Content-Length": strconv.Itoa(bigSize),
	} {
		if got := first.Header.Get(name); got != want {
			t.Errorf("streamed %s = %q, want %q", name, got, want)
		}
	}
	if got := hub.fetches.Load(); got != 1 {
		t.Fatalf("the upstream was asked %d times in all", got)
	}
}

// What the upstream sends does not match its digest. Every response in
// flight is cut short of the end, by an abort and not a clean close;
// nothing is stored; and the bytes that did leave cannot be completed into
// a file by asking for the rest.
func TestStreamAbortsWhenVerificationFails(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	hub.set(func() { hub.body = tamperedFile })
	logs := &regtest.LogBuffer{}
	cfg := streaming(hub)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	local := regtest.Start(t, cfg)
	fileURL := local.URL + bigPath
	// All but the last bytes: more than may be sent unverified, and not
	// enough for the fetch to finish and be judged.
	hub.release(bigSize - 100)

	whole := get(t, fileURL, nil)
	ranged := get(t, fileURL, map[string]string{"Range": "bytes=100-"})
	if whole.StatusCode != http.StatusOK || ranged.StatusCode != http.StatusPartialContent {
		t.Fatalf("statuses %d and %d", whole.StatusCode, ranged.StatusCode)
	}
	// Each is sent everything up to the part held back, and no further.
	if got := take(t, whole, sendable); got != tamperedFile[:sendable] {
		t.Fatal("the bytes sent are not the ones the upstream sent")
	}
	if got := take(t, ranged, sendable-100); got != tamperedFile[100:sendable] {
		t.Fatal("the ranged bytes sent are not the ones the upstream sent")
	}

	hub.release(bigSize) // the fetch completes, and fails verification
	for name, resp := range map[string]*http.Response{"whole": whole, "ranged": ranged} {
		more, err := rest(t, resp)
		if err == nil {
			t.Errorf("%s: the response ended cleanly after a failed verification", name)
		}
		if more != "" {
			t.Errorf("%s: %d bytes of the held-back end were sent", name, len(more))
		}
	}
	nothingKept(t, local)
	if got := logs.String(); !strings.Contains(got, "aborted a download") || !strings.Contains(got, "verification failed") {
		t.Fatalf("the abort and its reason are not in the log:\n%s", got)
	}

	// The clients each hold two mebibytes of a file that was never
	// verified, and will ask for the rest.
	resume := map[string]string{"Range": fmt.Sprintf("bytes=%d-", sendable)}
	t.Run("while the upstream is still wrong, the retry fails with a status", func(t *testing.T) {
		resp := get(t, fileURL, resume)
		if msg := body(t, resp); resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "verification failed") {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		nothingKept(t, local)
	})

	hub.set(func() { hub.body = bigFile })
	t.Run("a resumed download is sent the whole file, not the rest of it", func(t *testing.T) {
		resp := get(t, fileURL, resume)
		got, err := rest(t, resp)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Range") != "" || err != nil || got != bigFile {
			t.Fatalf("status %d, Content-Range %q, %d bytes, %v", resp.StatusCode, resp.Header.Get("Content-Range"), len(got), err)
		}
	})
	t.Run("and still is once the file is stored", func(t *testing.T) {
		until(t, "the file recorded as stored", func() bool { return stored(t, local.Reg) })
		before := hub.fetches.Load()
		for _, rng := range []string{resume["Range"], "bytes=5-9", "bytes=-100"} {
			resp := get(t, fileURL, map[string]string{"Range": rng})
			if got, err := rest(t, resp); resp.StatusCode != http.StatusOK || err != nil || got != bigFile {
				t.Fatalf("Range %s: status %d, %d bytes, %v", rng, resp.StatusCode, len(got), err)
			}
		}
		if hub.fetches.Load() != before {
			t.Fatal("a stored file was fetched again")
		}
	})
	t.Run("other files are resumed as usual", func(t *testing.T) {
		resp := get(t, local.URL+"/acme/big/resolve/main/config.json", map[string]string{"Range": "bytes=2-11"})
		if got := body(t, resp); resp.StatusCode != http.StatusPartialContent || got != `{"model_type":"big"}`[2:12] {
			t.Fatalf("status %d: %q", resp.StatusCode, got)
		}
	})
}

// A fetch that fails for another reason after bytes were sent leaves a
// client holding unverified bytes just the same.
func TestStreamAbortsWhenTheUpstreamFailsMidway(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	// The connection breaks after a mebibyte, and resuming is refused.
	hub.set(func() { hub.cutAt = mib })
	logs := &regtest.LogBuffer{}
	cfg := streaming(hub)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	local := regtest.Start(t, cfg)
	hub.release(mib)

	resp := get(t, local.URL+bigPath, nil)
	if got := take(t, resp, mib); resp.StatusCode != http.StatusOK || got != bigFile[:mib] {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if more, err := rest(t, resp); err == nil || more != "" {
		t.Fatalf("after the upstream failed: %d more bytes, %v", len(more), err)
	}
	nothingKept(t, local)
	if got := logs.String(); !strings.Contains(got, "aborted a download") || !strings.Contains(got, "upstream") {
		t.Fatalf("the abort and its reason are not in the log:\n%s", got)
	}

	// The upstream recovers. The client's mebibyte is not built on.
	hub.set(func() { hub.cutAt = 0 })
	hub.release(bigSize)
	retry := get(t, local.URL+bigPath, map[string]string{"Range": fmt.Sprintf("bytes=%d-", mib)})
	if got, err := rest(t, retry); retry.StatusCode != http.StatusOK || err != nil || got != bigFile {
		t.Fatalf("the retry: status %d, %d bytes, %v", retry.StatusCode, len(got), err)
	}
}

// A client that leaves takes nothing with it: the fetch carries on, and
// the clients that stay, or come later, get the file from that one fetch.
func TestStreamSurvivesAClientLeaving(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	local := regtest.Start(t, streaming(hub))
	hub.release(bigSize / 2)

	ctx, leave := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.URL+bigPath, nil)
	var starter *http.Response
	within(t, "an answer for the client that starts the fetch", func() {
		var err error
		if starter, err = http.DefaultClient.Do(req); err != nil {
			t.Error(err)
		}
	})
	if starter == nil {
		t.FailNow()
	}
	if got := take(t, starter, 1000); got != bigFile[:1000] {
		t.Fatal("wrong bytes")
	}
	stayer := get(t, local.URL+bigPath, nil)
	if got := take(t, stayer, mib); got != bigFile[:mib] {
		t.Fatal("wrong bytes")
	}

	leave()
	_ = starter.Body.Close()
	hub.release(bigSize)
	if got, err := rest(t, stayer); err != nil || got != bigFile[mib:] {
		t.Fatalf("the client that stayed: %d bytes, %v", len(got), err)
	}
	until(t, "the file recorded as stored", func() bool { return stored(t, local.Reg) })
	if got := hub.fetches.Load(); got != 1 {
		t.Fatalf("the upstream was asked for the file %d times, want once", got)
	}
}

// Stopping the server ends a response that is being sent from a fetch: the
// fetch is cancelled, and the response with it, short of the end.
func TestStreamEndsWhenTheServerStops(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	logs := &regtest.LogBuffer{}
	cfg := streaming(hub)
	cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	handler := server.New(reg, cfg)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
		_ = reg.Close()
	})
	hub.release(mib)

	resp := get(t, srv.URL+bigPath, nil)
	if got := take(t, resp, mib); got != bigFile[:mib] {
		t.Fatal("wrong bytes")
	}
	// Only the server's own work is stopped. The listener stays up, so it
	// is that, and not a closed socket, which ends the response.
	within(t, "the server stopping its fetches", func() {
		if err := handler.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if more, err := rest(t, resp); err == nil || len(more) != 0 {
		t.Fatalf("after shutdown: %d more bytes, %v", len(more), err)
	}
	if stored(t, reg) {
		t.Fatal("a cancelled fetch was recorded")
	}
	if got := logs.String(); !strings.Contains(got, "aborted a download") || !strings.Contains(got, "shutting down") {
		t.Fatalf("the abort and its reason are not in the log:\n%s", got)
	}
}

// With downloads redirected to the object store, the first request cannot
// be: nothing is in the bucket yet. It is sent from the registry while the
// file is fetched, and the next one is redirected.
func TestStreamBeforeTheObjectStoreHasTheFile(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	fake := blobtest.NewFakeS3(t)
	cfg := streaming(hub)
	cfg.RedirectDownloads = true
	local := regtest.StartWith(t, cfg, fake.Store(t))
	hub.release(bigSize / 2)

	first := get(t, local.URL+bigPath, nil)
	etag := `"` + kit.SHA256Hex(bigFile) + `"`
	if first.StatusCode != http.StatusOK || first.Header.Get("Location") != "" || first.Header.Get("ETag") != etag ||
		first.Header.Get("X-Linked-Etag") != etag || first.Header.Get("X-Linked-Size") != strconv.Itoa(bigSize) {
		t.Fatalf("first request: status %d, headers %v", first.StatusCode, first.Header)
	}
	start := take(t, first, mib)
	if len(fake.Keys(t)) != 0 {
		t.Fatalf("the bucket holds %v while the file is still arriving", fake.Keys(t))
	}
	hub.release(bigSize)
	if end, err := rest(t, first); err != nil || start+end != bigFile {
		t.Fatalf("first request: %d bytes, %v", len(start)+len(end), err)
	}

	until(t, "the file recorded as stored", func() bool { return stored(t, local.Reg) })
	next := get(t, local.URL+bigPath, nil)
	loc := next.Header.Get("Location")
	if next.StatusCode != http.StatusFound || !strings.HasPrefix(loc, fake.Config.Endpoint) || next.Header.Get("X-Linked-Etag") != etag {
		t.Fatalf("next request: status %d, Location %q", next.StatusCode, loc)
	}
	if direct := get(t, loc, nil); body(t, direct) != bigFile {
		t.Fatal("the bucket does not hold the file's bytes")
	}
}

// The setting off: nothing is sent until the file is stored and verified,
// as before.
func TestStreamingTurnedOff(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	cfg := streaming(hub)
	cfg.PullThroughNoStream = true
	local := regtest.Start(t, cfg)
	hub.release(bigSize - 100)

	// A client with a short timeout, as download libraries have, gets no
	// answer at all while the upstream holds the last bytes back.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.URL+bigPath, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("answered with %d before the file was fetched", resp.StatusCode)
	}
	if hub.fetches.Load() != 1 {
		t.Fatalf("the upstream was asked %d times", hub.fetches.Load())
	}

	hub.release(bigSize)
	resp := get(t, local.URL+bigPath, nil)
	if got, err := rest(t, resp); resp.StatusCode != http.StatusOK || err != nil || got != bigFile {
		t.Fatalf("status %d, %d bytes, %v", resp.StatusCode, len(got), err)
	}
	if hub.fetches.Load() != 1 {
		t.Fatalf("the upstream was asked %d times; the retry should have joined the fetch", hub.fetches.Load())
	}

	// And a failed verification is an ordinary 502, with nothing sent.
	t.Run("a mismatch is answered with a status", func(t *testing.T) {
		hub := newPacedHub(t)
		hub.set(func() { hub.body = tamperedFile })
		hub.release(bigSize)
		cfg := streaming(hub)
		cfg.PullThroughNoStream = true
		local := regtest.Start(t, cfg)
		resp := get(t, local.URL+bigPath, nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status %d, want 502", resp.StatusCode)
		}
		nothingKept(t, local)
	})
}

// What can be decided before a byte is sent is still answered with a
// status: the size limit, an upstream that fails, a disk that is full.
func TestStreamFailuresBeforeTheFirstByte(t *testing.T) {
	t.Parallel()

	t.Run("a file over the size limit is refused and not fetched", func(t *testing.T) {
		t.Parallel()
		hub := newPacedHub(t)
		hub.release(bigSize)
		cfg := streaming(hub)
		cfg.PullThroughMaxFileBytes = bigSize - 1
		local := regtest.Start(t, cfg)
		resp := get(t, local.URL+bigPath, nil)
		if msg := body(t, resp); resp.StatusCode != http.StatusForbidden || !strings.Contains(msg, "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES") {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		if hub.fetches.Load() != 0 {
			t.Fatal("a file over the limit was fetched")
		}
	})

	t.Run("the upstream refusing the file is 502 with the reason", func(t *testing.T) {
		t.Parallel()
		hub := newPacedHub(t)
		hub.set(func() { hub.refuse = http.StatusForbidden })
		local := regtest.Start(t, streaming(hub))
		resp := get(t, local.URL+bigPath, nil)
		if msg := body(t, resp); resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "could not fetch from upstream") {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		// Nothing was sent, so nothing is distrusted: once the upstream
		// serves the file, a ranged request is served as one.
		hub.set(func() { hub.refuse = 0 })
		hub.release(bigSize)
		part := get(t, local.URL+bigPath, map[string]string{"Range": "bytes=100-"})
		if got, err := rest(t, part); part.StatusCode != http.StatusPartialContent || err != nil || got != bigFile[100:] {
			t.Fatalf("afterwards: status %d, %d bytes, %v", part.StatusCode, len(got), err)
		}
	})

	t.Run("the registry's own disk failing is 500 and says no more", func(t *testing.T) {
		t.Parallel()
		hub := newPacedHub(t)
		hub.release(bigSize)
		disk := &fullDisk{Store: newFS(t)}
		disk.full.Store(true)
		logs := &regtest.LogBuffer{}
		cfg := streaming(hub)
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
		local := regtest.StartWith(t, cfg, disk)
		resp := get(t, local.URL+bigPath, nil)
		if msg := body(t, resp); resp.StatusCode != http.StatusInternalServerError || strings.Contains(msg, serverPath) {
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
		if !strings.Contains(logs.String(), "no space left on device") {
			t.Fatalf("the reason is not in the log:\n%s", logs.String())
		}
	})
}

// Requests that do not run from some byte to the end of the file are not
// sent from the fetch: they wait for the stored file, which answers them
// in full. A HEAD is answered from the listing and starts nothing.
func TestStreamLeavesOtherRequestsToTheStoredFile(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	local := regtest.Start(t, streaming(hub))
	fileURL := local.URL + bigPath

	head := do(t, http.MethodHead, fileURL, "", nil, nil)
	if head.StatusCode != http.StatusOK || head.ContentLength != bigSize || hub.fetches.Load() != 0 {
		t.Fatalf("HEAD: status %d, length %d, %d fetches", head.StatusCode, head.ContentLength, hub.fetches.Load())
	}

	hub.release(bigSize)
	etag := `"` + kit.SHA256Hex(bigFile) + `"`
	for name, tc := range map[string]struct {
		header map[string]string
		status int
		want   string
	}{
		"a bounded range":      {map[string]string{"Range": "bytes=10-19"}, http.StatusPartialContent, bigFile[10:20]},
		"a suffix":             {map[string]string{"Range": "bytes=-10"}, http.StatusPartialContent, bigFile[bigSize-10:]},
		"a range past the end": {map[string]string{"Range": fmt.Sprintf("bytes=%d-", bigSize)}, http.StatusRequestedRangeNotSatisfiable, ""},
		"a matching ETag":      {map[string]string{"If-None-Match": etag}, http.StatusNotModified, ""},
	} {
		resp := get(t, fileURL, tc.header)
		got, err := rest(t, resp)
		if resp.StatusCode != tc.status || err != nil || (tc.want != "" && got != tc.want) {
			t.Errorf("%s: status %d, %d bytes, %v", name, resp.StatusCode, len(got), err)
		}
	}
	if got := hub.fetches.Load(); got != 1 {
		t.Fatalf("the upstream was asked for the file %d times, want once", got)
	}
}

// A streamed download is a file transfer: the time limit on small request
// bodies does not apply to it, however slowly the upstream sends.
func TestStreamIsNotCutByTheBodyDeadline(t *testing.T) {
	t.Parallel()
	hub := newPacedHub(t)
	cfg := streaming(hub)
	cfg.BodyTimeout = 20 * time.Millisecond
	local := regtest.Start(t, cfg)
	hub.release(mib)
	resp := get(t, local.URL+bigPath, nil)
	start := take(t, resp, mib)
	// A connection's deadline is the operating system's clock, so this
	// waits on it: several times the limit, with the response open.
	time.Sleep(10 * cfg.BodyTimeout)
	hub.release(bigSize)
	if end, err := rest(t, resp); err != nil || start+end != bigFile {
		t.Fatalf("%d bytes, %v", len(start)+len(end), err)
	}
}
