// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// What the real client does with a file that pull-through is still
// fetching: with a slow upstream, and with an upstream whose content does
// not match its digest. The upstream is a local registry behind a handler
// that decides how one file is sent; nothing leaves this machine.

const mib = 1 << 20

// numbered returns size bytes of numbered lines: any two stretches of it
// differ, so bytes that ended up in the wrong place show.
func numbered(size int) string {
	var b strings.Builder
	b.Grow(size + 16)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "%015d\n", i)
	}
	return b.String()[:size]
}

// fetchPy downloads one file with hf_hub_download and reports what came of
// it: the file's digest, or the exception.
const fetchPy = `
import hashlib, json, os, sys, time
from huggingface_hub import hf_hub_download

repo, name = sys.argv[1], sys.argv[2]
started = time.monotonic()
try:
    path = hf_hub_download(repo, name)
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    out = {"ok": True, "sha256": digest.hexdigest(), "size": os.path.getsize(path)}
except Exception as e:
    out = {"ok": False, "error": type(e).__module__ + "." + type(e).__name__, "message": str(e)[:600]}
out["seconds"] = round(time.monotonic() - started, 2)
print(json.dumps(out))
`

type fetchResult struct {
	OK      bool    `json:"ok"`
	SHA256  string  `json:"sha256"`
	Size    int64   `json:"size"`
	Error   string  `json:"error"`
	Message string  `json:"message"`
	Seconds float64 `json:"seconds"`
}

// hfFetch runs hf_hub_download for one file against endpoint, with the
// client's cache in home and extra settings in its environment.
func hfFetch(t *testing.T, endpoint, home, repo, name string, env ...string) fetchResult {
	t.Helper()
	stdout, stderr, err := runPython(t, endpoint, "", fetchPy, append([]string{"HF_HOME=" + home}, env...), repo, name)
	if err != nil {
		t.Fatalf("the script itself failed: %v\n%s", err, stderr)
	}
	var res fetchResult
	lastJSON(t, stdout, &res)
	t.Logf("hf_hub_download(%s, %s): %+v", repo, name, res)
	return res
}

// oneFileHub is an upstream hub holding one repository, whose large file
// is sent the way send says. send gets the number of the request for that
// file, from 1, and writes the body.
type oneFileHub struct {
	url     string
	pushed  client.PushResult
	content string

	mu       sync.Mutex
	requests int
	// finished is when a send function said it had written the last byte
	// of the file (markFinished).
	finished time.Time
	// sent is closed when the registry under test has written a client all
	// of the file but the end it holds back.
	sent     chan struct{}
	sentOnce sync.Once
}

const (
	hubRepo = "acme/streamed"
	hubFile = "weights.bin"
)

func newOneFileHub(t *testing.T, content string, send func(h *oneFileHub, n int, w http.ResponseWriter, r *http.Request)) *oneFileHub {
	t.Helper()
	inst := regtest.Start(t, server.Config{AnonymousRead: true})
	dir := regtest.WriteTree(t, map[string]string{"config.json": `{"model_type":"streamed"}`, hubFile: content})
	pushed, err := client.New(inst.URL, inst.Write).Push(context.Background(), dir, hubRepo, client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(inst.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := &oneFileHub{pushed: pushed, content: content, sent: make(chan struct{})}
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/resolve/") || !strings.HasSuffix(r.URL.Path, "/"+hubFile) {
			proxy.ServeHTTP(w, r)
			return
		}
		h.mu.Lock()
		h.requests++
		n := h.requests
		h.mu.Unlock()
		send(h, n, w, r)
	}))
	t.Cleanup(front.Close)
	h.url = front.URL
	return h
}

func (h *oneFileHub) fetches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests
}

// markFinished notes that the upstream has written the last byte of the
// file, which is the earliest the registry can have verified it.
func (h *oneFileHub) markFinished() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.finished = time.Now()
}

func (h *oneFileHub) finishedAt() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.finished
}

// clientHasAllButTheEnd is called by the registry under test (startWatched)
// when one of its responses has been written everything of the file that
// may be sent before it is verified.
func (h *oneFileHub) clientHasAllButTheEnd() { h.sentOnce.Do(func() { close(h.sent) }) }

// sendAll writes body as the whole answer.
func sendAll(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
}

// sendAndHoldTheEnd writes body, but its last bytes only once the registry
// has passed its client everything it may before the file is verified. A
// local fetch is otherwise over, and judged, before the registry has sent
// its client more than a part of that, and how much would be chance.
func (h *oneFileHub) sendAndHoldTheEnd(w http.ResponseWriter, r *http.Request, body string) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	end := len(body) - 100
	if _, err := io.WriteString(w, body[:end]); err != nil {
		return
	}
	_ = http.NewResponseController(w).Flush()
	select {
	case <-h.sent:
	case <-r.Context().Done():
		return
	}
	_, _ = io.WriteString(w, body[end:])
}

// sendPaced writes body a piece at a time, with a pause after each piece
// but the last. It reports whether it wrote all of body.
func sendPaced(w http.ResponseWriter, r *http.Request, body string, piece int, pause time.Duration) bool {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	flusher := http.NewResponseController(w)
	for sent := 0; sent < len(body); sent += piece {
		if _, err := io.WriteString(w, body[sent:min(sent+piece, len(body))]); err != nil {
			return false
		}
		_ = flusher.Flush()
		if sent+piece >= len(body) {
			return true
		}
		select {
		case <-time.After(pause):
		case <-r.Context().Done():
			return false
		}
	}
	return true
}

// seen is one request for the large file as the registry answered it.
type seen struct {
	Range  string
	Status int
}

func (s seen) String() string {
	if s.Range == "" {
		return strconv.Itoa(s.Status)
	}
	return fmt.Sprintf("%d to Range %s", s.Status, s.Range)
}

// watched is a registry with pull-through from hub, which notes how each
// GET for the large file was answered.
type watched struct {
	url string
	reg *registry.Registry

	mu   sync.Mutex
	gets []seen
	// requests counts the GETs for the large file as they arrive, whether
	// or not they were ever answered, and firstByte is when the first byte
	// of the file was written to any of them.
	requests  int
	firstByte time.Time
}

type statusNoter struct {
	http.ResponseWriter
	note func(status int)
	once sync.Once
	// wrote is told how many bytes of the body have been written so far.
	wrote   func(total int)
	written int
}

func (w *statusNoter) WriteHeader(status int) {
	w.once.Do(func() { w.note(status) })
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusNoter) Write(p []byte) (int, error) {
	w.once.Do(func() { w.note(http.StatusOK) })
	n, err := w.ResponseWriter.Write(p)
	w.written += n
	w.wrote(w.written)
	return n, err
}

// Unwrap lets the handler flush through this wrapper.
func (w *statusNoter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func startWatched(t *testing.T, hub *oneFileHub, cfg server.Config) *watched {
	t.Helper()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AnonymousRead, cfg.Upstream, cfg.PullThrough = true, hub.url, true
	handler := server.New(reg, cfg)
	in := &watched{reg: reg}
	// Everything of the file that may be sent before it is verified: all
	// but the mebibyte the registry holds back.
	sendable := len(hub.content) - mib
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+hubFile) {
			asked := r.Header.Get("Range") // as the client sent it
			answered := 0                  // the status this request was answered with
			in.mu.Lock()
			in.requests++
			in.mu.Unlock()
			w = &statusNoter{ResponseWriter: w,
				note: func(status int) {
					in.mu.Lock()
					defer in.mu.Unlock()
					in.gets = append(in.gets, seen{Range: asked, Status: status})
					answered = status
				},
				wrote: func(total int) {
					// Only the body of a 200 or 206 is the file; an error
					// answer's body is not.
					if total > 0 && (answered == http.StatusOK || answered == http.StatusPartialContent) {
						in.mu.Lock()
						if in.firstByte.IsZero() {
							in.firstByte = time.Now()
						}
						in.mu.Unlock()
					}
					if total >= sendable {
						hub.clientHasAllButTheEnd()
					}
				}}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		// The server's own work first, then the listener, then the registry.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = handler.Shutdown(ctx)
		srv.Close()
		_ = reg.Close()
	})
	in.url = srv.URL
	return in
}

// answers is how the GETs for the large file were answered, in order.
func (in *watched) answers() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := make([]string, len(in.gets))
	for i, g := range in.gets {
		out[i] = g.String()
	}
	return strings.Join(out, "; ")
}

// arrived is how many GETs for the large file the registry received, and
// when it wrote the first byte of the file to one of them (zero if never).
func (in *watched) arrived() (int, time.Time) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.requests, in.firstByte
}

// isStored reports whether the large file has been fetched and recorded.
func (in *watched) isStored() bool {
	c, err := in.reg.Resolve(context.Background(), hubRepo, "main")
	if err != nil {
		return false
	}
	f, err := in.reg.File(context.Background(), c, hubFile)
	return err == nil && !f.Pending
}

func (in *watched) waitStored(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !in.isStored() {
		if time.Now().After(deadline) {
			t.Fatal("the fetch never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cached lists the files in a client's cache that could pass for the large
// file: anything under blobs/ or snapshots/ that is not a download in
// progress, with its size.
func cached(t *testing.T, home string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, home))
		if !strings.Contains(rel, "/blobs/") && !strings.Contains(rel, "/snapshots/") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[rel] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestHubClientAgainstASlowUpstream is the reason serve-while-fetching
// exists. The upstream takes about three seconds to send a file, and the
// client's download timeout (ten seconds by default) is set to one, which
// stands for a file of many gigabytes against the real timeout.
//
// With streaming off the registry sends nothing until the file is stored,
// so the client's first GET gets no byte within its timeout. What the
// client does then depends on its version: huggingface_hub 1.24.0 gives up
// with a read timeout, and later versions (from about 1.31 on) retry a
// timed-out download a few times, so they recover by retrying once the
// fetch has finished. With streaming on, no client needs to retry: the
// download succeeds in one request, because bytes reach it while the
// registry is still fetching.
//
// The test therefore pins what the registry does (how many GETs arrived,
// and when the first byte of the file left it, against when the upstream
// finished sending), not a client's retry policy.
func TestHubClientAgainstASlowUpstream(t *testing.T) {
	python(t)
	content := numbered(8 * mib)
	// A quarter of a mebibyte every tenth of a second: about three seconds
	// in all, and the mebibyte held back at the end takes well under one.
	slow := func(h *oneFileHub, _ int, w http.ResponseWriter, r *http.Request) {
		if sendPaced(w, r, content, mib/4, 100*time.Millisecond) {
			h.markFinished()
		}
	}
	const timeout = "HF_HUB_DOWNLOAD_TIMEOUT=1"

	t.Run("streaming off: nothing is sent before the fetch has finished", func(t *testing.T) {
		t.Parallel()
		hub := newOneFileHub(t, content, slow)
		local := startWatched(t, hub, server.Config{PullThroughNoStream: true})
		home := t.TempDir()

		first := hfFetch(t, local.url, home, hubRepo, hubFile, timeout)
		gets, firstByte := local.arrived()
		// The first GET got nothing in time: the client either failed or
		// had to ask again.
		if first.OK && gets < 2 {
			t.Fatalf("the download succeeded in %d request although nothing could be sent for three seconds: %+v", gets, first)
		}
		if first.OK && first.SHA256 != kit.SHA256Hex(content) {
			t.Fatalf("the client retried and ended with the wrong file: %+v", first)
		}
		if !first.OK {
			if files := cached(t, home); len(files) != 0 {
				t.Errorf("the failed download left files in the cache: %v", files)
			}
			// The fetch carried on without the client, and a retry once it
			// has finished is served from the registry.
			local.waitStored(t)
			second := hfFetch(t, local.url, home, hubRepo, hubFile, timeout)
			if !second.OK || second.SHA256 != kit.SHA256Hex(content) {
				t.Fatalf("the retry after the fetch finished: %+v", second)
			}
			_, firstByte = local.arrived()
		}
		finished := hub.finishedAt()
		if finished.IsZero() || firstByte.IsZero() {
			t.Fatalf("the upstream finished at %v and the first byte was sent at %v", finished, firstByte)
		}
		if firstByte.Before(finished) {
			t.Errorf("a byte of the file was sent %v before the upstream had finished sending it", finished.Sub(firstByte))
		}
		if hub.fetches() != 1 {
			t.Errorf("the upstream was asked for the file %d times, want once", hub.fetches())
		}
		t.Logf("%d GETs; the registry answered: %s", gets, local.answers())
	})

	t.Run("streaming on: one request, sent while it is fetched", func(t *testing.T) {
		t.Parallel()
		hub := newOneFileHub(t, content, slow)
		local := startWatched(t, hub, server.Config{})
		res := hfFetch(t, local.url, t.TempDir(), hubRepo, hubFile, timeout)
		if !res.OK || res.SHA256 != kit.SHA256Hex(content) || res.Size != int64(len(content)) {
			t.Fatalf("the download: %+v", res)
		}
		// One request, answered once, with the whole file: no timeout and
		// no resumed transfer hides behind the success.
		gets, firstByte := local.arrived()
		if gets != 1 {
			t.Errorf("the registry received %d GETs for the file, want one", gets)
		}
		if got := local.answers(); got != "200" {
			t.Errorf("the registry answered %q, want a single 200", got)
		}
		// The upstream takes about three seconds; the first byte left the
		// registry long before the end of that.
		finished := hub.finishedAt()
		if finished.IsZero() || firstByte.IsZero() || finished.Sub(firstByte) < time.Second {
			t.Errorf("the first byte was sent at %v and the upstream finished at %v: not well before", firstByte, finished)
		}
		if hub.fetches() != 1 {
			t.Errorf("the upstream was asked for the file %d times, want once", hub.fetches())
		}
	})
}

// TestHubClientAgainstAnUpstreamThatSendsTheWrongBytes is the price of
// sending before verifying, and what keeps it from being paid. The
// upstream's content does not match the digest in its listing. The file is
// twelve mebibytes because huggingface_hub takes a download in pieces of
// ten: with less, it has kept nothing by the time the transfer is cut, and
// has nothing to resume from.
//
// The client is sent eleven mebibytes before the registry can know, the
// transfer is cut, and the client, holding ten of them, asks for the rest.
// It must never end up with a complete file made of those ten.
func TestHubClientAgainstAnUpstreamThatSendsTheWrongBytes(t *testing.T) {
	python(t)
	content := numbered(12 * mib)
	tampered := content[:100] + "TAMPERED" + content[108:]
	want := kit.SHA256Hex(content)
	const kept = "bytes=10485760-" // what a client holding ten mebibytes asks for

	t.Run("the upstream stays wrong, then is corrected", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		corrected := false
		hub := newOneFileHub(t, content, func(h *oneFileHub, n int, w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			body := tampered
			if corrected {
				body = content
			}
			mu.Unlock()
			if n == 1 {
				h.sendAndHoldTheEnd(w, r, body)
				return
			}
			sendAll(w, body)
		})
		local := startWatched(t, hub, server.Config{})
		home := t.TempDir()

		first := hfFetch(t, local.url, home, hubRepo, hubFile)
		if first.OK {
			t.Fatalf("the client has a file although the upstream's bytes are wrong: %+v", first)
		}
		// The transfer was cut, the client asked for the rest, and that was
		// refused with the reason: the registry had by then fetched the file
		// again, in full, and found it wrong again.
		if got := local.answers(); got != "200; 502 to Range "+kept {
			t.Errorf("the registry answered %q", got)
		}
		if first.Error != "huggingface_hub.errors.HfHubHTTPError" || !strings.Contains(first.Message, "verification failed") {
			t.Errorf("the client failed with %s: %s", first.Error, first.Message)
		}
		if files := cached(t, home); len(files) != 0 {
			t.Fatalf("the failed download left files in the client's cache: %v", files)
		}
		if local.isStored() {
			t.Fatal("the registry recorded the file")
		}

		mu.Lock()
		corrected = true
		mu.Unlock()
		second := hfFetch(t, local.url, home, hubRepo, hubFile)
		if !second.OK || second.SHA256 != want {
			t.Fatalf("after the upstream was corrected: %+v, want sha256 %s", second, want)
		}
		t.Logf("the registry answered: %s", local.answers())
	})

	// The case that would go wrong: the upstream is sound again by the time
	// the client asks for the rest. Sending the rest would complete a file
	// whose first ten mebibytes are the wrong ones.
	t.Run("the upstream is corrected before the client resumes", func(t *testing.T) {
		t.Parallel()
		hub := newOneFileHub(t, content, func(h *oneFileHub, n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				h.sendAndHoldTheEnd(w, r, tampered)
				return
			}
			sendAll(w, content)
		})
		local := startWatched(t, hub, server.Config{})
		home := t.TempDir()

		res := hfFetch(t, local.url, home, hubRepo, hubFile)
		// The client asked for the rest and was sent the whole file, which
		// it took in place of what it had.
		if got := local.answers(); got != "200; 200 to Range "+kept {
			t.Errorf("the registry answered %q", got)
		}
		if !res.OK || res.Size != int64(len(content)) {
			t.Fatalf("the download: %+v", res)
		}
		if res.SHA256 != want {
			t.Fatalf("the client ended with a complete file that is not the genuine one: sha256 %s, want %s", res.SHA256, want)
		}
		for name, size := range cached(t, home) {
			if size != int64(len(content)) {
				t.Errorf("the cache holds %s with %d bytes", name, size)
			}
		}
	})
}
