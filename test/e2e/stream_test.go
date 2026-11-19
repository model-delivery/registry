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
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// What the real client does with a file that pull-through is still
// fetching from a slow upstream. The upstream is a local registry behind a
// handler that decides how one file is sent; nothing leaves this machine.

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
	h := &oneFileHub{pushed: pushed, content: content}
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

// sendPaced writes body a piece at a time, with a pause after each.
func sendPaced(w http.ResponseWriter, r *http.Request, body string, piece int, pause time.Duration) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	flusher := http.NewResponseController(w)
	for sent := 0; sent < len(body); sent += piece {
		if _, err := io.WriteString(w, body[sent:min(sent+piece, len(body))]); err != nil {
			return
		}
		_ = flusher.Flush()
		select {
		case <-time.After(pause):
		case <-r.Context().Done():
			return
		}
	}
}

// isStored reports whether the hub's large file has been fetched and
// recorded by in.
func isStored(in *regtest.Instance) bool {
	c, err := in.Reg.Resolve(context.Background(), hubRepo, "main")
	if err != nil {
		return false
	}
	f, err := in.Reg.File(context.Background(), c, hubFile)
	return err == nil && !f.Pending
}

// cached lists the files in a client's cache that could pass for the large
// file: anything under blobs/ or snapshots/, with its size.
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

// TestHubClientAgainstASlowUpstream reproduces what the first download of
// a large file through pull-through does to a real client. The upstream
// takes about three seconds to send a file, and the client's download
// timeout (ten seconds by default) is set to one, which stands for a file
// of many gigabytes against the real timeout.
//
// The registry sends nothing until the file is stored, so the client's
// first attempt times out; only a retry after the fetch has finished
// succeeds.
func TestHubClientAgainstASlowUpstream(t *testing.T) {
	python(t)
	content := numbered(8 * mib)
	// A quarter of a mebibyte every tenth of a second: about three seconds
	// in all.
	hub := newOneFileHub(t, content, func(_ *oneFileHub, _ int, w http.ResponseWriter, r *http.Request) {
		sendPaced(w, r, content, mib/4, 100*time.Millisecond)
	})
	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.url, PullThrough: true})
	home := t.TempDir()
	const timeout = "HF_HUB_DOWNLOAD_TIMEOUT=1"

	first := hfFetch(t, local.URL, home, hubRepo, hubFile, timeout)
	if first.OK {
		t.Fatalf("the download succeeded although nothing could be sent for three seconds: %+v", first)
	}
	// What huggingface_hub 1.24.0 does: the read timeout of the GET, about
	// a second in, and no retry of its own.
	if first.Error != "httpx.ReadTimeout" {
		t.Errorf("the client failed with %s (%s), not a read timeout", first.Error, first.Message)
	}
	if first.Seconds > 2.5 {
		t.Errorf("the client took %.1fs to give up with a one-second timeout", first.Seconds)
	}
	if files := cached(t, home); len(files) != 0 {
		t.Errorf("the failed download left files in the cache: %v", files)
	}
	// The fetch carried on without the client, and a retry once it has
	// finished is served from the registry.
	deadline := time.Now().Add(30 * time.Second)
	for !isStored(local) {
		if time.Now().After(deadline) {
			t.Fatal("the fetch never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := hfFetch(t, local.URL, home, hubRepo, hubFile, timeout)
	if !second.OK || second.SHA256 != kit.SHA256Hex(content) {
		t.Fatalf("the retry after the fetch finished: %+v", second)
	}
	if hub.fetches() != 1 {
		t.Errorf("the upstream was asked for the file %d times, want once", hub.fetches())
	}
}
