// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// sample is one reading of /metrics: each line that is not a comment, by
// everything before its value.
type sample map[string]float64

// The series the tests below wait on.
const (
	httpInFlight = "modelregistry_http_requests_in_flight"
	jobsInFlight = "modelregistry_pullthrough_fetches_in_flight"
	jobsWaiting  = "modelregistry_pullthrough_fetches_waiting"
)

func parseMetrics(t *testing.T, text string) sample {
	t.Helper()
	out := sample{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cut := strings.LastIndexByte(line, ' ')
		if cut < 0 {
			t.Fatalf("a metrics line with no value: %q", line)
		}
		v, err := strconv.ParseFloat(line[cut+1:], 64)
		if err != nil {
			t.Fatalf("a metrics line whose value is not a number: %q", line)
		}
		if _, twice := out[line[:cut]]; twice {
			t.Fatalf("the series %s is written twice", line[:cut])
		}
		out[line[:cut]] = v
	}
	return out
}

// idle is the state between the steps of a test: nothing in flight but the
// request reading the metrics, and no pull-through job left.
func idle(s sample) bool {
	return s[httpInFlight] == 1 && s[jobsInFlight] == 0 && s[jobsWaiting] == 0
}

// scraper reads a registry's metrics the way Prometheus would.
type scraper struct {
	url, token string
	// clock, when set, is the registry's clock. It is moved on before each
	// reading, past the time the registry keeps its counts of what it holds.
	clock *fakeClock
}

// read reads the metrics until ready says the registry is in the state the
// test is waiting for, and returns that reading and its text. A request is
// counted when its handler returns, which can be a moment after its client
// has the whole answer; waiting for the in-flight count closes that gap.
func (sc scraper) read(t *testing.T, ready func(sample) bool) (sample, string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if sc.clock != nil {
			sc.clock.Advance(time.Minute)
		}
		resp := do(t, http.MethodGet, sc.url+"/metrics", sc.token, nil, nil)
		text := body(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /metrics: status %d: %s", resp.StatusCode, text)
		}
		if got := resp.Header.Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
			t.Fatalf("Content-Type = %q", got)
		}
		s := parseMetrics(t, text)
		if ready(s) {
			return s, text
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry never reached the state waited for; last reading:\n%s", text)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The series a comparison leaves out: those of the requests that read the
// metrics, of which there are as many as it took, and the buckets and sums
// of durations, which are not the same twice.
func compared(series string) bool {
	return !strings.Contains(series, `route="probe"`) &&
		!strings.HasPrefix(series, "modelregistry_http_request_duration_seconds_bucket") &&
		!strings.HasPrefix(series, "modelregistry_http_request_duration_seconds_sum")
}

// moved checks that between two readings exactly the series in want
// changed, each by exactly the amount given, and nothing else did. Series
// in loose changed by an amount the caller checks itself.
func moved(t *testing.T, step string, before, after sample, want map[string]float64, loose ...string) {
	t.Helper()
	got := map[string]float64{}
	for series, v := range after {
		if d := v - before[series]; d != 0 && compared(series) && !slices.Contains(loose, series) {
			got[series] = d
		}
	}
	for series := range before {
		if _, still := after[series]; !still {
			t.Errorf("%s: the series %s disappeared", step, series)
		}
	}
	var wrong []string
	for series, d := range want {
		if got[series] != d {
			wrong = append(wrong, fmt.Sprintf("  %s moved by %v, want %v", series, got[series], d))
		}
	}
	for series, d := range got {
		if _, expected := want[series]; !expected {
			wrong = append(wrong, fmt.Sprintf("  %s moved by %v, and nothing in this step should move it", series, d))
		}
	}
	if len(wrong) > 0 {
		slices.Sort(wrong)
		t.Fatalf("%s:\n%s", step, strings.Join(wrong, "\n"))
	}
}

// requests is the series counting requests of one kind.
func requests(route, method, code string) string {
	return fmt.Sprintf(`modelregistry_http_requests_total{route=%q,method=%q,code=%q}`, route, method, code)
}

// timed is the series counting the timed requests of a class of route.
func timed(route string) string {
	return fmt.Sprintf(`modelregistry_http_request_duration_seconds_count{route=%q}`, route)
}

// The names of the series the tests expect to move.
const (
	mHit           = `modelregistry_download_requests_total{cache="hit"}`
	mMiss          = `modelregistry_download_requests_total{cache="miss"}`
	mRedirects     = `modelregistry_download_redirects_total`
	mBytesStore    = `modelregistry_download_bytes_total{source="store"}`
	mBytesFetch    = `modelregistry_download_bytes_total{source="fetch"}`
	mListStarted   = `modelregistry_pullthrough_listings_started_total`
	mFetchStarted  = `modelregistry_pullthrough_fetches_started_total`
	mFetchedBytes  = `modelregistry_pullthrough_fetched_bytes_total`
	mQueueFull     = `modelregistry_pullthrough_queue_full_total`
	mStreams       = `modelregistry_pullthrough_streams_started_total`
	mStreamsCut    = `modelregistry_pullthrough_streams_aborted_total`
	mDistrusted    = `modelregistry_pullthrough_distrusted_files`
	mUploadBlobs   = `modelregistry_upload_blobs_total`
	mUploadBytes   = `modelregistry_upload_bytes_total`
	mMirrorStarted = `modelregistry_mirrors_started_total`
	mMirrorBytes   = `modelregistry_mirror_fetched_bytes_total`
	mRepos         = `modelregistry_repositories`
	mRevisions     = `modelregistry_revisions`
	mBlobs         = `modelregistry_storage_blobs{backend="filesystem"}`
	mBlobBytes     = `modelregistry_storage_bytes{backend="filesystem"}`
	mPending       = `modelregistry_pending_files`
	mPendingBytes  = `modelregistry_pending_bytes`
	mBackupAt      = `modelregistry_backup_last_success_timestamp_seconds`
	mBackupSize    = `modelregistry_backup_last_size_bytes`
	mPruneFailures = `modelregistry_backup_prune_failures_total`
	mVerifyAt      = `modelregistry_verify_last_run_timestamp_seconds`
)

func mListings(outcome string) string {
	return fmt.Sprintf(`modelregistry_pullthrough_listings_total{outcome=%q}`, outcome)
}

func mFetches(outcome string) string {
	return fmt.Sprintf(`modelregistry_pullthrough_fetches_total{outcome=%q}`, outcome)
}

func mCommits(source string) string {
	return fmt.Sprintf(`modelregistry_commits_total{source=%q}`, source)
}

func mMirrors(outcome string) string {
	return fmt.Sprintf(`modelregistry_mirrors_total{outcome=%q}`, outcome)
}

func mBackups(outcome string) string {
	return fmt.Sprintf(`modelregistry_backups_total{outcome=%q}`, outcome)
}

func mVerifyRuns(outcome string) string {
	return fmt.Sprintf(`modelregistry_verify_runs_total{outcome=%q}`, outcome)
}

func mVerifyFound(status string) string {
	return fmt.Sprintf(`modelregistry_verify_last_run_blobs{status=%q}`, status)
}

// misbehavingHub is an upstream hub that can be told to go wrong, one file
// at a time.
type misbehavingHub struct {
	url string
	// altered holds the suffixes of the paths answered with other content
	// than the hub lists, and gone those answered "not found".
	altered, gone sync.Map
	// hold, while it holds a channel, keeps the downloads of the files
	// under shards/ waiting until the channel is closed.
	hold atomic.Pointer[chan struct{}]
	// cutShort is the path suffix of the one file that is sent in two
	// parts: its true beginning, and, once finish is closed, an ending that
	// is not its own.
	cutShort string
	content  string
	finish   chan struct{}
}

func (h *misbehavingHub) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/") || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		suffix := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/'):]
		if _, ok := h.gone.Load(suffix); ok {
			http.NotFound(w, r)
			return
		}
		if _, ok := h.altered.Load(suffix); ok {
			_, _ = w.Write([]byte("not what the hub lists"))
			return
		}
		if strings.Contains(r.URL.Path, "/shards/") {
			if hold := h.hold.Load(); hold != nil {
				select {
				case <-*hold:
				case <-r.Context().Done():
					return
				}
			}
		}
		if h.cutShort != "" && suffix == h.cutShort {
			w.Header().Set("Content-Length", strconv.Itoa(len(h.content)))
			const first = 256 << 10
			_, _ = w.Write([]byte(h.content[:first]))
			_ = http.NewResponseController(w).Flush()
			select {
			case <-h.finish:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte(strings.Repeat("!", len(h.content)-first)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Names no metric may ever show: the test looks for them in the output.
const (
	hubRepo     = "okapi-labs/quagga-7b"
	mirrorRepo  = "okapi-labs/numbat-mini"
	badRepo     = "okapi-labs/dunnart-bad"
	ownRepo     = "wombat-team/echidna"
	absentRepo  = "okapi-labs/thylacine"
	scrapeToken = "gnu-scraper"
)

// TestMetricsCountWhatTheRegistryDoes drives a registry through the things
// an operator watches (a push, downloads from the store, pull-through hits
// and misses, fetches that fail in each way, a full queue, mirrors, a
// backup, a verify run) and after each checks that the counters for it
// moved by exactly what happened, and that nothing else moved.
func TestMetricsCountWhatTheRegistryDoes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// What the upstream holds. Sizes on either side of the mebibyte that
	// decides whether a file is sent while it is fetched.
	const shards = 68 // the four fetches that run at once, and the 64 that may wait
	config := `{"model_type":"quagga"}`
	weights := strings.Repeat("quagga weights ", 2000)
	large := strings.Repeat("0123456789abcdef", (1<<20+300<<10)/16)
	cut := strings.Repeat("fedcba9876543210", (1<<20+512<<10)/16)
	hubFiles := map[string]string{
		"quagga-config.json":         config,
		"quagga-weights.safetensors": weights,
		"quagga-large.bin":           large,
		"quagga-cut.bin":             cut,
		"quagga-gone.bin":            "the hub lists this and then loses it",
		"quagga-spare.bin":           "never asked for until the queue is full",
	}
	var hubBytes, shardBytes int64
	for i := range shards {
		content := fmt.Sprintf("shard %03d of quagga", i)
		hubFiles[fmt.Sprintf("shards/quagga-%03d.bin", i)] = content
		shardBytes += int64(len(content))
	}
	for _, content := range hubFiles {
		hubBytes += int64(len(content))
	}
	mirrorFiles := map[string]string{"numbat-config.json": `{"model_type":"numbat"}`, "numbat-weights.bin": strings.Repeat("numbat", 500)}
	mirrorBytes := int64(len(mirrorFiles["numbat-config.json"]) + len(mirrorFiles["numbat-weights.bin"]))

	origin := regtest.Start(t, server.Config{AnonymousRead: true})
	hubPushed := push(t, origin, hubRepo, hubFiles, client.PushOptions{})
	push(t, origin, mirrorRepo, mirrorFiles, client.PushOptions{})
	push(t, origin, badRepo, map[string]string{"dunnart-weights.bin": "the only file, and the hub alters it"}, client.PushOptions{})
	hub := &misbehavingHub{cutShort: "/quagga-cut.bin", content: cut, finish: make(chan struct{})}
	hub.altered.Store("/dunnart-weights.bin", true)
	target, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0) // a fetch the registry gives up on is not news
	front := httptest.NewServer(hub.wrap(proxy))
	t.Cleanup(front.Close)
	hub.url = front.URL

	// The registry under test: private, so that every request needs a token.
	clock := newFakeClock()
	started := clock.Now()
	in := regtest.Start(t, server.Config{Upstream: hub.url, PullThrough: true, Now: clock.Now, Version: "v9.8.7-test"})
	scrapeSecret, err := in.Reg.CreateToken(ctx, scrapeToken, registry.RoleRead)
	if err != nil {
		t.Fatal(err)
	}
	metricsOf := scraper{url: in.URL, token: scrapeSecret, clock: clock}
	get := func(path string, header map[string]string) *http.Response {
		t.Helper()
		return do(t, http.MethodGet, in.URL+path, in.Read, nil, header)
	}
	file := func(repo, path string) string { return "/" + repo + "/resolve/main/" + path }

	before, _ := metricsOf.read(t, idle)
	// step reads the metrics once the registry is idle again and checks
	// what moved since the last reading.
	step := func(name string, want map[string]float64, loose ...string) sample {
		t.Helper()
		after, _ := metricsOf.read(t, idle)
		moved(t, name, before, after, want, loose...)
		before = after
		return after
	}

	// ---- before anything has happened ----
	if before[`modelregistry_build_info{version="v9.8.7-test"}`] != 1 {
		t.Fatalf("no build info for the version: %v", before)
	}
	if got, want := before["modelregistry_process_start_time_seconds"], float64(started.UnixMilli())/1000; got != want {
		t.Fatalf("process start time = %v, want %v", got, want)
	}
	for _, zero := range []string{mHit, mMiss, mRedirects, mBytesStore, mBytesFetch, mListStarted, mListings("succeeded"), mListings("upstream"),
		mFetchStarted, mFetches("succeeded"), mFetches("upstream"), mFetches("verification"), mFetches("local"), mFetches("cancelled"),
		mFetchedBytes, jobsInFlight, jobsWaiting, mQueueFull, mStreams, mStreamsCut, mDistrusted, mUploadBlobs, mUploadBytes,
		mCommits("push"), mCommits("mirror"), mCommits("pull_through"), mMirrorStarted, mMirrors("succeeded"), mMirrors("failed"), mMirrorBytes,
		mRepos, mRevisions, mBlobs, mBlobBytes, mPending, mPendingBytes, mBackupAt, mBackupSize, mBackups("succeeded"), mBackups("failed"),
		mPruneFailures, mVerifyAt, mVerifyRuns("succeeded"), mVerifyFound("ok"), mVerifyFound("missing"), mVerifyFound("damaged"),
		timed("api"), `modelregistry_http_request_duration_seconds_bucket{route="api",le="+Inf"}`} {
		if v, written := before[zero]; !written || v != 0 {
			t.Errorf("%s = %v (written: %v) before anything has happened; want it written, at 0", zero, v, written)
		}
	}

	// ---- requests that are refused ----
	if resp := do(t, http.MethodGet, in.URL+"/api/v1/repos", "", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a request without a token: status %d", resp.StatusCode)
	}
	if resp := do(t, http.MethodGet, in.URL+"/metrics", "", nil, nil); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("the metrics without a token, on a private registry: status %d", resp.StatusCode)
	}
	step("a request without a token", map[string]float64{requests("api", "GET", "4xx"): 1, timed("api"): 1})

	// ---- a push ----
	own := map[string]string{"wombat-a.txt": "aaaaa", "wombat-b.bin": "bbbbbbbbbb"}
	push(t, in, ownRepo, own, client.PushOptions{})
	step("a push of two files", map[string]float64{
		requests("api", "HEAD", "4xx"):   2, // is it stored? no
		requests("upload", "PUT", "2xx"): 2,
		requests("api", "POST", "2xx"):   1, // the commit
		timed("api"):                     3,
		mUploadBlobs:                     2,
		mUploadBytes:                     15,
		mCommits("push"):                 1,
		mRepos:                           1,
		mRevisions:                       1,
		mBlobs:                           2,
		mBlobBytes:                       15,
	})

	// ---- downloads of what is stored ----
	if got := body(t, get(file(ownRepo, "wombat-b.bin"), nil)); got != own["wombat-b.bin"] {
		t.Fatalf("downloaded %q", got)
	}
	if resp := do(t, http.MethodHead, in.URL+file(ownRepo, "wombat-b.bin"), in.Read, nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: status %d", resp.StatusCode)
	}
	if got := body(t, get(file(ownRepo, "wombat-b.bin"), map[string]string{"Range": "bytes=2-5"})); got != "bbbb" {
		t.Fatalf("a range of it: %q", got)
	}
	step("two downloads and a HEAD of a stored file", map[string]float64{
		requests("download", "GET", "2xx"):  2,
		requests("download", "HEAD", "2xx"): 1,
		mHit:                                2, // a HEAD is not a download
		mBytesStore:                         10 + 4,
	})

	// ---- pull-through: a miss, then a hit ----
	if got := body(t, get(file(hubRepo, "quagga-config.json"), nil)); got != config {
		t.Fatalf("first download through the cache: %q", got)
	}
	if got := body(t, get(file(hubRepo, "quagga-config.json"), nil)); got != config {
		t.Fatalf("second download through the cache: %q", got)
	}
	step("a first download of an upstream file, and a second", map[string]float64{
		requests("download", "GET", "2xx"): 2,
		mListStarted:                       1,
		mListings("succeeded"):             1,
		mCommits("pull_through"):           1,
		mFetchStarted:                      1,
		mFetches("succeeded"):              1,
		mFetchedBytes:                      float64(len(config)),
		mMiss:                              1,
		mHit:                               1,
		mBytesStore:                        2 * float64(len(config)),
		mRepos:                             1,
		mRevisions:                         1,
		mBlobs:                             1,
		mBlobBytes:                         float64(len(config)),
		mPending:                           float64(len(hubFiles) - 1),
		mPendingBytes:                      float64(hubBytes) - float64(len(config)),
	})

	// ---- a file large enough to be sent while it is fetched ----
	if got := body(t, get(file(hubRepo, "quagga-large.bin"), nil)); got != large {
		t.Fatalf("the large file arrived as %d bytes, want %d", len(got), len(large))
	}
	after := step("a first download of a large file", map[string]float64{
		requests("download", "GET", "2xx"): 1,
		mMiss:                              1,
		mFetchStarted:                      1,
		mFetches("succeeded"):              1,
		mFetchedBytes:                      float64(len(large)),
		mBlobs:                             1,
		mBlobBytes:                         float64(len(large)),
		mPending:                           -1,
		mPendingBytes:                      -float64(len(large)),
	}, mStreams, mBytesFetch, mBytesStore)
	// It is sent from the fetch in flight; if the fetch won the race to its
	// end, it is sent from the store. Either way every byte is counted once.
	streamed := after[mStreams]
	switch {
	case streamed == 1 && after[mBytesFetch] == float64(len(large)):
	case streamed == 0 && after[mBytesFetch] == 0:
	default:
		t.Fatalf("%v responses streamed, %v bytes sent from a fetch", streamed, after[mBytesFetch])
	}
	storeSoFar := float64(10 + 4 + 2*len(config))
	if got := after[mBytesStore] + after[mBytesFetch]; got != storeSoFar+float64(len(large)) {
		t.Fatalf("%v bytes counted as sent in all, want %v", got, storeSoFar+float64(len(large)))
	}

	// ---- a fetch whose content does not match what the upstream listed ----
	hub.altered.Store("/quagga-weights.safetensors", true)
	if resp := get(file(hubRepo, "quagga-weights.safetensors"), nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a file the upstream alters: status %d", resp.StatusCode)
	}
	hub.altered.Delete("/quagga-weights.safetensors")
	step("a fetch that fails verification", map[string]float64{
		requests("download", "GET", "5xx"): 1,
		mMiss:                              1,
		mFetchStarted:                      1,
		mFetches("verification"):           1,
	})

	// ---- a fetch the upstream fails ----
	hub.gone.Store("/quagga-gone.bin", true)
	if resp := get(file(hubRepo, "quagga-gone.bin"), nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a file the upstream no longer has: status %d", resp.StatusCode)
	}
	step("a fetch the upstream fails", map[string]float64{
		requests("download", "GET", "5xx"): 1,
		mMiss:                              1,
		mFetchStarted:                      1,
		mFetches("upstream"):               1,
	})

	// ---- a repository the upstream does not have ----
	if resp := get("/api/models/"+absentRepo, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a repository nobody has: status %d", resp.StatusCode)
	}
	step("a listing the upstream does not have", map[string]float64{
		requests("hf", "GET", "4xx"): 1,
		timed("hf"):                  1,
		mListStarted:                 1,
		mListings("not_found"):       1,
	})

	// ---- a download cut off because the fetch it was sent from failed ----
	resp := get(file(hubRepo, "quagga-cut.bin"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a file sent while it is fetched: status %d", resp.StatusCode)
	}
	head := make([]byte, 1024)
	if _, err := io.ReadFull(resp.Body, head); err != nil || string(head) != cut[:len(head)] {
		t.Fatalf("the beginning of the file did not arrive while the upstream was still sending: %v", err)
	}
	close(hub.finish) // the upstream now sends an ending that is not the file's
	if rest, err := io.ReadAll(resp.Body); err == nil {
		t.Fatalf("the download ended cleanly after %d more bytes, although its content never verified", len(rest))
	}
	after = step("a download cut off when its fetch failed", map[string]float64{
		requests("download", "GET", "2xx"): 1, // the status had been sent
		mMiss:                              1,
		mFetchStarted:                      1,
		mFetches("verification"):           1,
		mStreams:                           1,
		mStreamsCut:                        1,
		mDistrusted:                        1,
	}, mBytesFetch)
	sentBeforeCut := after[mBytesFetch] - streamed*float64(len(large))
	if sentBeforeCut < float64(len(head)) || sentBeforeCut > float64(len(cut)-1<<20) {
		t.Fatalf("%v bytes counted as sent before the cut; the client read %d, and the last mebibyte of %d is never sent unverified", sentBeforeCut, len(head), len(cut))
	}

	// ---- more fetches than the registry takes on ----
	held := make(chan struct{})
	hub.hold.Store(&held)
	var wg sync.WaitGroup
	for i := range shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("shards/quagga-%03d.bin", i)
			resp := get(file(hubRepo, name), nil)
			if got := body(t, resp); resp.StatusCode != http.StatusOK || got != hubFiles[name] {
				t.Errorf("%s: status %d, %q", name, resp.StatusCode, got)
			}
		}()
	}
	full, _ := metricsOf.read(t, func(s sample) bool {
		return s[jobsInFlight] == 4 && s[jobsWaiting] == 64 && s[httpInFlight] == shards+1
	})
	moved(t, "68 first downloads held by the upstream", before, full, map[string]float64{
		httpInFlight:  shards,
		jobsInFlight:  4,
		jobsWaiting:   64,
		mMiss:         shards,
		mFetchStarted: shards,
	})
	before = full
	resp = get(file(hubRepo, "quagga-spare.bin"), nil)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("one fetch more than the queue holds: status %d", resp.StatusCode)
	}
	refused, _ := metricsOf.read(t, func(s sample) bool { return s[httpInFlight] == shards+1 })
	moved(t, "a request refused because the queue is full", before, refused, map[string]float64{
		requests("download", "GET", "5xx"): 1,
		mMiss:                              1,
		mQueueFull:                         1,
	})
	before = refused
	close(held)
	wg.Wait()
	hub.hold.Store(nil)
	step("the held downloads finish", map[string]float64{
		requests("download", "GET", "2xx"): shards,
		httpInFlight:                       -shards,
		jobsInFlight:                       -4,
		jobsWaiting:                        -64,
		mFetches("succeeded"):              shards,
		mFetchedBytes:                      float64(shardBytes),
		mBytesStore:                        float64(shardBytes),
		mBlobs:                             shards,
		mBlobBytes:                         float64(shardBytes),
		mPending:                           -shards,
		mPendingBytes:                      -float64(shardBytes),
	})

	// ---- mirrors ----
	mirror := func(repo string) (lastEvent string) {
		t.Helper()
		resp := do(t, http.MethodPost, in.URL+"/api/v1/mirror", in.Write, strings.NewReader(`{"repo":"`+repo+`"}`), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("mirror %s: status %d: %s", repo, resp.StatusCode, body(t, resp))
		}
		lines := strings.Split(strings.TrimSpace(body(t, resp)), "\n")
		var last struct{ Event string }
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
			t.Fatal(err)
		}
		return last.Event
	}
	if got := mirror(mirrorRepo); got != "done" {
		t.Fatalf("the mirror ended with %q", got)
	}
	step("a mirror", map[string]float64{
		requests("task", "POST", "2xx"): 1,
		mMirrorStarted:                  1,
		mMirrors("succeeded"):           1,
		mMirrorBytes:                    float64(mirrorBytes),
		mCommits("mirror"):              1,
		mRepos:                          1,
		mRevisions:                      1,
		mBlobs:                          2,
		mBlobBytes:                      float64(mirrorBytes),
	})
	if got := mirror(badRepo); got != "error" {
		t.Fatalf("the mirror of altered content ended with %q", got)
	}
	step("a mirror that fails", map[string]float64{
		requests("task", "POST", "2xx"): 1, // the failure is the last line of a 200
		mMirrorStarted:                  1,
		mMirrors("failed"):              1,
	})

	// ---- a backup ----
	resp = do(t, http.MethodPost, in.URL+"/api/v1/backups", in.Admin, nil, nil)
	var taken struct {
		Backup struct {
			Size int64 `json:"size"`
		} `json:"backup"`
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("backup: status %d", resp.StatusCode)
	}
	decode(t, resp, &taken)
	after = step("a backup", map[string]float64{
		requests("api", "POST", "2xx"): 1,
		timed("api"):                   1,
		mBackups("succeeded"):          1,
		mBackupSize:                    float64(taken.Backup.Size),
	}, mBackupAt)
	if at := time.UnixMilli(int64(after[mBackupAt] * 1000)); time.Since(at) < 0 || time.Since(at) > time.Minute {
		t.Fatalf("the last backup is timed at %s, which is not just now", at)
	}

	// ---- a verify run, over storage that has been tampered with ----
	blobPath := func(content string) string {
		digest := kit.SHA256Hex(content)
		return filepath.Join(in.Data, "blobs", "sha256", digest[:2], digest)
	}
	if err := os.Remove(blobPath(own["wombat-a.txt"])); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath(own["wombat-b.bin"]), []byte("BBBBBBBBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp = do(t, http.MethodPost, in.URL+"/api/v1/verify", in.Admin, nil, nil)
	if report := body(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(report, `"event":"done"`) {
		t.Fatalf("verify: status %d: %s", resp.StatusCode, report)
	}
	stored := 2 + 1 + 1 + shards + 2 // pushed, the config, the large file, the shards, the mirror
	after = step("a verify run", map[string]float64{
		requests("task", "POST", "2xx"): 1,
		mVerifyRuns("succeeded"):        1,
		mVerifyFound("ok"):              float64(stored - 2),
		mVerifyFound("missing"):         1,
		mVerifyFound("damaged"):         1,
	}, mVerifyAt)
	if after[mVerifyAt] == 0 || after[mBlobs] != float64(stored) {
		t.Fatalf("after the run: timed at %v, %v blobs counted, want %d", after[mVerifyAt], after[mBlobs], stored)
	}

	// ---- and nothing in any of it says what the registry holds ----
	_, text := metricsOf.read(t, idle)
	secrets := []string{
		"okapi", "quagga", "numbat", "dunnart", "wombat", "echidna", "thylacine", "shards", ".bin", ".json", ".safetensors",
		scrapeToken, scrapeSecret, in.Read, in.Write, in.Admin, hubPushed.SHA,
		kit.SHA256Hex(config), kit.SHA256Hex(large), "127.0.0.1", "localhost", hub.url, in.URL,
	}
	for _, line := range strings.Split(text, "\n") {
		for _, secret := range secrets {
			if strings.Contains(line, secret) {
				t.Errorf("a metrics line contains %q, which names something the registry holds or who asked:\n  %s", secret, line)
			}
		}
	}
	// Every label value is one of a fixed few. A new label, or a new value
	// of one, is added here on purpose.
	allowed := map[string][]string{
		"version": {"v9.8.7-test"},
		"route":   {"download", "upload", "api", "task", "hf", "ui", "probe", "other"},
		"method":  {"GET", "HEAD", "POST", "PUT", "DELETE", "other"},
		"code":    {"1xx", "2xx", "3xx", "4xx", "5xx"},
		"cache":   {"hit", "miss"},
		"source":  {"store", "fetch", "push", "mirror", "pull_through"},
		"outcome": {"succeeded", "failed", "not_found", "upstream", "verification", "local", "cancelled"},
		"backend": {"filesystem", "s3", "other"},
		"status":  {"ok", "missing", "damaged", "unreadable"},
		"le":      {"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "30", "+Inf"},
	}
	series := 0
	for name := range parseMetrics(t, text) {
		series++
		if !strings.HasPrefix(name, "modelregistry_") {
			t.Errorf("%s does not carry the registry's prefix", name)
		}
		open := strings.IndexByte(name, '{')
		if open < 0 {
			continue
		}
		for _, pair := range strings.Split(strings.TrimSuffix(name[open+1:], "}"), ",") {
			label, value, _ := strings.Cut(pair, "=")
			if !slices.Contains(allowed[label], strings.Trim(value, `"`)) {
				t.Errorf("%s: the label %s has the value %s, which is not one of its fixed values", name, label, value)
			}
		}
	}
	// Seventy-five files were served and four repositories recorded; the
	// number of series is what it was for an empty registry, plus the kinds
	// of request seen since.
	if series > 250 {
		t.Errorf("%d series: the output has grown with what the registry holds", series)
	}
}

// With downloads redirected to the object store, a download is a hit and a
// redirect, and the registry sends none of the bytes itself.
func TestMetricsCountRedirectedDownloads(t *testing.T) {
	t.Parallel()
	fake := blobtest.NewFakeS3(t)
	clock := newFakeClock()
	in := regtest.StartWith(t, server.Config{AnonymousRead: true, RedirectDownloads: true, Now: clock.Now}, fake.Store(t))
	metricsOf := scraper{url: in.URL, clock: clock}
	before, _ := metricsOf.read(t, idle)
	push(t, in, "acme/tiny", modelFiles, client.PushOptions{})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp := do(t, method, in.URL+"/acme/tiny/resolve/main/model.safetensors", "", nil, nil)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") == "" {
			t.Fatalf("%s: status %d", method, resp.StatusCode)
		}
	}
	var total float64
	for _, content := range modelFiles {
		total += float64(len(content))
	}
	// What the registry holds is counted at most once in a while, however
	// often the metrics are read: a reading at the same moment as the last
	// still shows the counts taken then. What is counted as it happens is
	// never late.
	same, _ := scraper{url: in.URL}.read(t, idle)
	if same[mRepos] != 0 || same[`modelregistry_storage_blobs{backend="s3"}`] != 0 || same[mUploadBlobs] != float64(len(modelFiles)) {
		t.Fatalf("read again at once: %v repositories, %v stored files, %v uploads; want the counts of what is held as they were, and the uploads counted",
			same[mRepos], same[`modelregistry_storage_blobs{backend="s3"}`], same[mUploadBlobs])
	}
	after, _ := metricsOf.read(t, idle)
	moved(t, "a redirected download", before, after, map[string]float64{
		requests("api", "HEAD", "4xx"):      float64(len(modelFiles)),
		requests("upload", "PUT", "2xx"):    float64(len(modelFiles)),
		requests("api", "POST", "2xx"):      1,
		timed("api"):                        float64(len(modelFiles)) + 1,
		requests("download", "GET", "3xx"):  1,
		requests("download", "HEAD", "3xx"): 1,
		mHit:                                1,
		mRedirects:                          1, // the GET; a HEAD downloads nothing
		mUploadBlobs:                        float64(len(modelFiles)),
		mUploadBytes:                        total,
		mCommits("push"):                    1,
		mRepos:                              1,
		mRevisions:                          1,
		`modelregistry_storage_blobs{backend="s3"}`: float64(len(modelFiles)),
		`modelregistry_storage_bytes{backend="s3"}`: total,
	})
	if after[mBytesStore] != 0 || after[mBytesFetch] != 0 {
		t.Fatalf("%v bytes counted as sent by the registry, which sent none", after[mBytesStore]+after[mBytesFetch])
	}
}

// brokenBackups is a store whose backups can be made impossible to store,
// or to remove.
type brokenBackups struct {
	blob.Store
	cannotStore, cannotRemove atomic.Bool
}

func (b *brokenBackups) PutArchive(ctx context.Context, name, path string) error {
	if b.cannotStore.Load() {
		return errors.New("the bucket refuses writes")
	}
	return b.Store.PutArchive(ctx, name, path)
}

func (b *brokenBackups) DeleteArchive(ctx context.Context, name string) error {
	if b.cannotRemove.Load() {
		return errors.New("the bucket refuses deletes")
	}
	return b.Store.DeleteArchive(ctx, name)
}

// The age of the last backup is right from the first scrape after a
// restart, when this process has not taken one yet: it is the newest backup
// in storage. Backups that fail, and backups whose pruning fails, are
// counted apart.
func TestMetricsFollowBackups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &brokenBackups{Store: newFS(t)}
	// A backup an earlier run left in storage.
	left := filepath.Join(t.TempDir(), "left.db")
	const leftContent = "what an earlier run stored"
	if err := os.WriteFile(left, []byte(leftContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.PutArchive(ctx, "registry-20260101T000000Z.db", left); err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	in := regtest.StartWith(t, server.Config{AnonymousRead: true, BackupKeep: 1, Now: clock.Now}, store)
	metricsOf := scraper{url: in.URL, clock: clock}

	first, _ := metricsOf.read(t, idle)
	if at := time.UnixMilli(int64(first[mBackupAt] * 1000)); time.Since(at) < -time.Minute || time.Since(at) > time.Minute {
		t.Fatalf("with a backup in storage and none taken since the start, the last backup is timed at %s (%v)", at, first[mBackupAt])
	}
	if first[mBackupSize] != float64(len(leftContent)) || first[mBackups("succeeded")] != 0 {
		t.Fatalf("size %v, %v backups counted; want the stored backup's size and none counted", first[mBackupSize], first[mBackups("succeeded")])
	}

	backup := func(want int) {
		t.Helper()
		if resp := do(t, http.MethodPost, in.URL+"/api/v1/backups", in.Admin, nil, nil); resp.StatusCode != want {
			t.Fatalf("backup: status %d, want %d: %s", resp.StatusCode, want, body(t, resp))
		}
	}
	// Stored, and the old one removed.
	backup(http.StatusCreated)
	// Stored, and the old one could not be removed.
	store.cannotRemove.Store(true)
	backup(http.StatusCreated)
	// Not stored.
	store.cannotStore.Store(true)
	backup(http.StatusInternalServerError)
	// A scheduled backup is counted like one asked for: both are the
	// registry's Backup.
	store.cannotStore.Store(false)
	store.cannotRemove.Store(false)
	if _, err := in.Reg.Backup(ctx, 1); err != nil {
		t.Fatal(err)
	}

	after, _ := metricsOf.read(t, idle)
	moved(t, "four backups", first, after, map[string]float64{
		requests("api", "POST", "2xx"): 2,
		requests("api", "POST", "5xx"): 1,
		timed("api"):                   3,
		mBackups("succeeded"):          3,
		mBackups("failed"):             1,
		mPruneFailures:                 1,
	}, mBackupAt, mBackupSize)
	if after[mBackupSize] == float64(len(leftContent)) || after[mBackupSize] == 0 {
		t.Fatalf("the size of the last backup is still %v", after[mBackupSize])
	}
}
