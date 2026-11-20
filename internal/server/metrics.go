// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/metrics"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// GET /metrics: what the registry is doing, for Prometheus. docs/operations.md
// lists every metric and what it counts.
//
// Two rules hold for all of them:
//
//   - No label is ever taken from a request. Labels are the small fixed
//     sets declared below (a class of route, a method, a class of status, an
//     outcome, the storage backend), so the output says nothing about which
//     repositories the registry holds and does not grow with them.
//   - Counting costs a request a few atomic operations and never changes
//     its answer. Bytes are counted once per write, not per byte.

// The classes of route that requests are counted under.
const (
	// routeDownload is a file of a revision: GET and HEAD .../resolve/...
	routeDownload = "download"
	// routeUpload is a request whose body is model content (isFileTransfer).
	routeUpload = "upload"
	// routeAPI is the registry's own API, /api/v1, apart from routeTask.
	routeAPI = "api"
	// routeTask is the API requests that last as long as the work they
	// start: a mirror, a verify run, a snapshot download.
	routeTask = "task"
	// routeHF is the Hugging Face compatible metadata and upload-step routes.
	routeHF = "hf"
	// routeUI is the web UI and its static files.
	routeUI = "ui"
	// routeProbe is the health check and this endpoint.
	routeProbe = "probe"
	// routeOther is everything else: a path that is nothing.
	routeOther = "other"
)

// The values of the other labels.
const (
	outcomeSucceeded    = "succeeded"
	outcomeFailed       = "failed"
	outcomeNotFound     = "not_found"
	outcomeUpstream     = "upstream"
	outcomeVerification = "verification"
	outcomeLocal        = "local"
	outcomeCancelled    = "cancelled"

	cacheHit  = "hit"
	cacheMiss = "miss"

	fromStore = "store"
	fromFetch = "fetch"

	sourcePush        = "push"
	sourceMirror      = "mirror"
	sourcePullThrough = "pull_through"

	methodOther = "other"
)

// timedRoutes are the classes of route whose requests are timed: those
// that are not transfers and do not last as long as a job.
var timedRoutes = []string{routeAPI, routeHF, routeUI, routeProbe, routeOther}

// durationBuckets are the upper bounds, in seconds, of the request duration
// histogram.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// catalogueTTL is how long the counts of what the registry holds are kept
// between scrapes. Counting reads the database's tables, so it is not done
// for every scrape of every scraper.
const catalogueTTL = 15 * time.Second

// serverMetrics is every metric the server keeps. The zero value counts
// nothing, which is what a Server built without New has.
type serverMetrics struct {
	set *metrics.Set

	requests *metrics.CounterVec
	inFlight *metrics.Gauge
	duration map[string]*metrics.Histogram // by class of route; only timedRoutes

	downloadHit, downloadMiss *metrics.Counter
	downloadRedirects         *metrics.Counter
	bytesFromStore            *metrics.Counter
	bytesFromFetch            *metrics.Counter

	uploadBlobs, uploadBytes *metrics.Counter
	commits                  *metrics.CounterVec
	mirrorsStarted           *metrics.Counter
	mirrors                  *metrics.CounterVec
	mirrorBytes              *metrics.Counter

	pull pullMetrics

	// Checks of followed repositories (sync.go).
	syncChecks, syncChanges *metrics.Counter
	syncBytes               *metrics.Counter
	syncFailures            *metrics.CounterVec
	followed, syncOldest    *metrics.Gauge

	// Set when the metrics are written, from what the registry and the
	// pull-through coordinator hold.
	pullInFlight, pullWaiting, distrusted *metrics.Gauge
	repos, revisions                      *metrics.Gauge
	blobs, blobBytes                      *metrics.GaugeVec
	pendingFiles, pendingBytes            *metrics.Gauge
	backupLast, backupSize                *metrics.Gauge
	backups                               *metrics.CounterVec
	backupPruneFailures                   *metrics.Counter
	verifyLast                            *metrics.Gauge
	verifyFound                           *metrics.GaugeVec
	verifyRuns                            *metrics.CounterVec

	// mu guards what is remembered between scrapes.
	mu sync.Mutex
	// countedAt is when the registry's contents were last counted.
	countedAt time.Time
	// stored is the newest backup that was in storage when the server
	// started, which is the last successful backup until this process takes
	// one. storedKnown is set once storage has been asked.
	stored      registry.Backup
	storedKnown bool
	// oldestSync is the oldest last successful check among the followed
	// repositories (when it was followed, for one never checked well),
	// counted with the catalogue; zero when none is followed.
	oldestSync time.Time
}

// pullMetrics is what the pull-through coordinator counts. The zero value
// counts nothing.
type pullMetrics struct {
	listingsStarted *metrics.Counter
	listings        *metrics.CounterVec
	fetchesStarted  *metrics.Counter
	fetches         *metrics.CounterVec
	fetchedBytes    *metrics.Counter
	queueFull       *metrics.Counter
	streamsStarted  *metrics.Counter
	streamsAborted  *metrics.Counter
	// recorded is the commits counter's series for pull-through: a revision
	// recorded from an upstream listing.
	recorded *metrics.Counter
}

// declare makes every metric. The names here are the ones
// docs/operations.md lists; TestEveryMetricIsDocumented holds the two
// together.
func (m *serverMetrics) declare(version string, started time.Time) {
	set := metrics.New()
	m.set = set
	outcome := func(values ...string) metrics.Label { return metrics.L("outcome", values...) }
	routes := []string{routeDownload, routeUpload, routeAPI, routeTask, routeHF, routeUI, routeProbe, routeOther}

	set.Gauge("modelregistry_build_info", "The version of the registry that is running; the value is always 1.",
		metrics.L("version", version)).With(version).Set(1)
	set.Gauge("modelregistry_process_start_time_seconds", "When the server started, in seconds since the Unix epoch.").
		With().Set(unixSeconds(started))

	// HTTP.
	m.requests = set.SparseCounter("modelregistry_http_requests_total",
		"Requests answered, by class of route, method and class of status. A series appears once a request of its kind has been answered.",
		metrics.L("route", routes...),
		metrics.L("method", http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, methodOther),
		metrics.L("code", "1xx", "2xx", "3xx", "4xx", "5xx"))
	m.inFlight = set.Gauge("modelregistry_http_requests_in_flight", "Requests being answered now, the one reading this among them.").With()
	duration := set.Histogram("modelregistry_http_request_duration_seconds",
		"How long requests took, by class of route. File transfers (download, upload) and requests that last as long as the job they start (task) are not timed.",
		durationBuckets, metrics.L("route", timedRoutes...))
	m.duration = map[string]*metrics.Histogram{}
	for _, route := range timedRoutes {
		m.duration[route] = duration.With(route)
	}

	// Downloads.
	downloads := set.Counter("modelregistry_download_requests_total",
		"GET requests for a file, by whether its content was already stored (hit) or had to be fetched from the upstream first (miss). HEAD requests are not counted.",
		metrics.L("cache", cacheHit, cacheMiss))
	m.downloadHit, m.downloadMiss = downloads.With(cacheHit), downloads.With(cacheMiss)
	m.downloadRedirects = set.Counter("modelregistry_download_redirects_total",
		"GET requests for a file answered with a redirect to a signed URL on the object store. They are also counted as hits.").With()
	served := set.Counter("modelregistry_download_bytes_total",
		"Bytes of file content sent to clients by the registry itself: from storage (store), or from a pull-through fetch while it was in flight (fetch). Redirected downloads send none.",
		metrics.L("source", fromStore, fromFetch))
	m.bytesFromStore, m.bytesFromFetch = served.With(fromStore), served.With(fromFetch)

	// Pull-through.
	m.pull.listingsStarted = set.Counter("modelregistry_pullthrough_listings_started_total",
		"Lookups of a revision's file listing started, each for a revision the registry did not hold.").With()
	m.pull.listings = set.Counter("modelregistry_pullthrough_listings_total",
		"Listing lookups finished, by outcome: succeeded, not_found (the upstream does not have it, or it may not be recorded here), upstream (the upstream failed), local (the registry failed), cancelled (the server was stopping).",
		outcome(outcomeSucceeded, outcomeNotFound, outcomeUpstream, outcomeLocal, outcomeCancelled))
	m.pull.fetchesStarted = set.Counter("modelregistry_pullthrough_fetches_started_total",
		"Fetches of a file's content from the upstream started. Requests for a file already being fetched join that fetch and start none.").With()
	m.pull.fetches = set.Counter("modelregistry_pullthrough_fetches_total",
		"File fetches finished, by outcome: succeeded, upstream (unreachable, or it answered with an error), verification (the content did not match its digest or size), local (the registry failed, such as a full disk), cancelled (the server was stopping).",
		outcome(outcomeSucceeded, outcomeUpstream, outcomeVerification, outcomeLocal, outcomeCancelled))
	m.pull.fetchedBytes = set.Counter("modelregistry_pullthrough_fetched_bytes_total",
		"Bytes of file content downloaded from the upstream by fetches that succeeded.").With()
	m.pullInFlight = set.Gauge("modelregistry_pullthrough_fetches_in_flight",
		"Pull-through jobs running now: file fetches and listing lookups that hold a slot.").With()
	m.pullWaiting = set.Gauge("modelregistry_pullthrough_fetches_waiting",
		"Pull-through jobs waiting for a slot. A request that would add one past the limit is refused.").With()
	m.pull.queueFull = set.Counter("modelregistry_pullthrough_queue_full_total",
		"Requests answered 503 because the queue of waiting fetches was full.").With()
	m.pull.streamsStarted = set.Counter("modelregistry_pullthrough_streams_started_total",
		"Responses that began sending a file while it was still being fetched.").With()
	m.pull.streamsAborted = set.Counter("modelregistry_pullthrough_streams_aborted_total",
		"Such responses cut off because the fetch failed, the content did not verify, or the server stopped.").With()
	m.distrusted = set.Gauge("modelregistry_pullthrough_distrusted_files",
		"Files whose streamed fetch failed after bytes were sent: until restart they are verified before they are sent, and a request for part of one is answered with all of it.").With()

	// Uploads and mirrors.
	m.uploadBlobs = set.Counter("modelregistry_upload_blobs_total",
		"Files received from uploads and stored, content the registry already held among them.").With()
	m.uploadBytes = set.Counter("modelregistry_upload_bytes_total", "Bytes of the files received from uploads.").With()
	m.commits = set.Counter("modelregistry_commits_total",
		"Requests that recorded a revision, by where it came from: push (the registry API and Hugging Face uploads), mirror, pull_through (a listing recorded on demand).",
		metrics.L("source", sourcePush, sourceMirror, sourcePullThrough))
	m.pull.recorded = m.commits.With(sourcePullThrough)
	m.mirrorsStarted = set.Counter("modelregistry_mirrors_started_total", "Mirror requests that began copying a revision.").With()
	m.mirrors = set.Counter("modelregistry_mirrors_total", "Mirrors finished, by outcome.", outcome(outcomeSucceeded, outcomeFailed))
	m.mirrorBytes = set.Counter("modelregistry_mirror_fetched_bytes_total", "Bytes downloaded from the upstream by mirrors that succeeded.").With()

	// Keeping mirrors up to date.
	m.syncChecks = set.Counter("modelregistry_sync_checks_total",
		"Checks of a followed repository against its upstream that finished, scheduled and asked for, whatever came of them.").With()
	m.syncChanges = set.Counter("modelregistry_sync_changes_total",
		"Checks that added or moved a ref: a new revision mirrored, or a ref recorded at one already stored.").With()
	m.syncFailures = set.Counter("modelregistry_sync_failures_total",
		"Checks that failed, by outcome: upstream (unreachable, or it answered with an error), verification (content did not match its digest), not_found (the upstream does not have the repository or the followed branch), local (the registry failed).",
		outcome(outcomeUpstream, outcomeVerification, outcomeNotFound, outcomeLocal))
	m.syncBytes = set.Counter("modelregistry_sync_fetched_bytes_total", "Bytes downloaded from the upstream by checks of followed repositories.").With()
	m.followed = set.Gauge("modelregistry_followed_repositories", "Repositories on the follow list.").With()
	m.syncOldest = set.Gauge("modelregistry_sync_oldest_success_age_seconds",
		"Seconds since the followed repository that has gone longest without a successful check last had one (or was followed, if it never has); 0 when none is followed.").With()

	// Storage and catalogue.
	backend := metrics.L("backend", blob.KindFilesystem, blob.KindS3, blob.KindOther)
	m.repos = set.Gauge("modelregistry_repositories", "Repositories the registry holds.").With()
	m.revisions = set.Gauge("modelregistry_revisions", "Revisions recorded, over all repositories.").With()
	m.blobs = set.Gauge("modelregistry_storage_blobs", "Stored files, each distinct content counted once, by storage backend.", backend)
	m.blobBytes = set.Gauge("modelregistry_storage_bytes", "Total size of the stored files, by storage backend.", backend)
	m.pendingFiles = set.Gauge("modelregistry_pending_files", "Files that revisions list and whose content has not been fetched from the upstream.").With()
	m.pendingBytes = set.Gauge("modelregistry_pending_bytes", "Total size of those files.").With()

	// Backups and verify runs.
	m.backupLast = set.Gauge("modelregistry_backup_last_success_timestamp_seconds",
		"When the newest metadata backup was stored, in seconds since the Unix epoch; 0 when there is none.").With()
	m.backupSize = set.Gauge("modelregistry_backup_last_size_bytes", "Size of that backup.").With()
	m.backups = set.Counter("modelregistry_backups_total", "Metadata backups attempted since the server started, scheduled and on demand, by outcome.",
		outcome(outcomeSucceeded, outcomeFailed))
	m.backupPruneFailures = set.Counter("modelregistry_backup_prune_failures_total",
		"Backups that were stored and could not be followed by the removal of old ones.").With()
	m.verifyLast = set.Gauge("modelregistry_verify_last_run_timestamp_seconds",
		"When the last verify run that checked every stored file finished, in seconds since the Unix epoch; 0 when none has since the server started.").With()
	m.verifyFound = set.Gauge("modelregistry_verify_last_run_blobs", "What that run found, by state of the stored file.",
		metrics.L("status", registry.BlobOK, registry.BlobMissing, registry.BlobDamaged, registry.BlobUnreadable))
	m.verifyRuns = set.Counter("modelregistry_verify_runs_total",
		"Verify runs since the server started, by outcome: succeeded (every stored file was checked, whatever was found) or failed (the run stopped early).",
		outcome(outcomeSucceeded, outcomeFailed))
}

// unixSeconds is t as the format writes a time: seconds since the epoch.
// The zero time is 0, not a date in the year 1.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixMilli()) / 1000
}

// routeClass says which class of route a request is for. It reads the
// method and the shape of the path and returns one of the constants above,
// never anything from the request.
func routeClass(r *http.Request) string {
	path := r.URL.Path
	switch {
	case path == "/healthz" || path == "/metrics":
		return routeProbe
	case isFileTransfer(r):
		return routeUpload
	case strings.HasPrefix(path, "/api/v1/"):
		switch strings.TrimPrefix(path, "/api/v1/") {
		case "mirror", "verify", "backups/snapshot", "backup/snapshot":
			return routeTask
		}
		// Syncing one repository now lasts as long as the sync.
		if r.Method == http.MethodPost && strings.HasSuffix(path, "/sync") && path != "/api/v1/sync" {
			return routeTask
		}
		return routeAPI
	case strings.HasPrefix(path, "/api/"):
		return routeHF
	case path == "/" || strings.HasPrefix(path, "/ui/") || strings.HasPrefix(path, "/static/"):
		return routeUI
	}
	// What root routes, parsed the way root parses it.
	segs, ok := splitPath(r.URL.EscapedPath())
	if !ok {
		return routeOther
	}
	switch parseRootPath(segs).kind {
	case rootResolve:
		return routeDownload
	case rootLFSBatch:
		return routeHF
	case rootRepoLink:
		return routeUI // a hub-style link to a repository page
	}
	return routeOther
}

// methodLabel is the request's method if it is one the registry answers,
// and "other" for anything else a client may send.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete:
		return method
	}
	return methodOther
}

// statusClass is the class of a status: "2xx" for 200 to 299.
func statusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	}
	return "5xx"
}

// instrument counts every request: how many are in flight, and, once one
// has been answered, its class of route, method and class of status, and
// for the routes that are timed how long it took. It is the outermost
// handler, so a request refused before it reaches a route is counted too.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := &s.m
		route := routeClass(r)
		seen := &statusWriter{ResponseWriter: w}
		started := time.Now() // not the server's clock: a duration is real time
		m.inFlight.Add(1)
		// Deferred, so that a response aborted by a panic is counted as well.
		// The request leaves the in-flight count last: once it has, it has
		// been counted.
		defer func() {
			m.requests.With(route, methodLabel(r.Method), statusClass(seen.code())).Inc()
			m.duration[route].Observe(time.Since(started).Seconds())
			m.inFlight.Add(-1)
		}()
		next.ServeHTTP(seen, r)
	})
}

// statusWriter notes the status a handler answers with and changes nothing
// else: writes, a file sent with ReadFrom, and everything reached through
// http.ResponseController go to the writer underneath.
//
// One thing does not pass through a wrapper. http.MaxBytesReader tells the
// server to close the connection after an oversized body through a method
// only the server's own writer has; the places that call it hand it that
// writer (baseWriter).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status // informational answers (1xx) come before the real one
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// ReadFrom keeps the server's own way of sending a file (sendfile, where
// the system has it), which a writer without this method would lose.
func (w *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return readFrom(w.ResponseWriter, src)
}

// Unwrap is how http.ResponseController reaches the writer underneath.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// code is the status that was answered: 200 when the handler wrote nothing,
// as the server then does.
func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// readFrom copies src to w, with w's own ReadFrom when it has one.
func readFrom(w http.ResponseWriter, src io.Reader) (int64, error) {
	if rf, ok := w.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	// Only the Write method, or io.Copy would look for ReadFrom again.
	return io.Copy(struct{ io.Writer }{w}, src)
}

// baseWriter is the server's own writer underneath any wrappers, for
// http.MaxBytesReader (see statusWriter).
func baseWriter(w http.ResponseWriter) http.ResponseWriter {
	for {
		wrapped, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = wrapped.Unwrap()
	}
}

// countedWriter adds the bytes of a response body to a counter, and like
// statusWriter changes nothing else.
type countedWriter struct {
	http.ResponseWriter
	sent *metrics.Counter
}

func (w *countedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.sent.Add(int64(n))
	return n, err
}

func (w *countedWriter) ReadFrom(src io.Reader) (int64, error) {
	n, err := readFrom(w.ResponseWriter, src)
	w.sent.Add(n)
	return n, err
}

func (w *countedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// pullOutcome says how a pull-through job ended, for its metric. A listing
// that the upstream does not have, or that may not be recorded here, is
// "not found"; for a file fetch there is no such outcome, and an upstream
// that no longer has a file it listed has failed.
func pullOutcome(err error, listing bool) string {
	switch {
	case err == nil:
		return outcomeSucceeded
	case errors.Is(err, errShuttingDown), errors.Is(err, context.Canceled):
		return outcomeCancelled
	case errors.Is(err, blob.ErrMismatch):
		return outcomeVerification
	case listing && (errors.Is(err, mirror.ErrUpstreamNotFound) || errors.Is(err, registry.ErrInvalid) || errors.Is(err, registry.ErrLocalRepo)):
		return outcomeNotFound
	case mirror.FromUpstream(err):
		return outcomeUpstream
	}
	return outcomeLocal
}

// serveMetrics writes the metrics. It is gated like any route that reads.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request) {
	s.readGauges(r.Context())
	var out bytes.Buffer
	if err := s.m.set.Write(&out); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}

// readGauges sets the gauges that show what is kept elsewhere: by the
// pull-through coordinator, and by the registry.
//
// What the registry holds is counted with one statement over the database,
// at most once in catalogueTTL however often the metrics are read. Storage
// is asked one thing, once: which backup is the newest, so that the age of
// the last backup is right from the first scrape after a restart. Nothing
// here lists or reads stored files.
func (s *Server) readGauges(ctx context.Context) {
	m := &s.m
	if p := s.puller; p != nil {
		p.mu.Lock()
		running, waiting, distrusted := p.running, p.waiting, len(p.distrusted)
		p.mu.Unlock()
		m.pullInFlight.Set(float64(running - waiting))
		m.pullWaiting.Set(float64(waiting))
		m.distrusted.Set(float64(distrusted))
	}
	if s.reg == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := s.now()
	if m.countedAt.IsZero() || now.Sub(m.countedAt) >= catalogueTTL || now.Before(m.countedAt) {
		m.countedAt = now
		if t, err := s.reg.Totals(ctx); err != nil {
			// The gauges keep what they had. Tried again after the TTL.
			s.log.Warn("metrics: could not count what the registry holds", "error", err)
		} else {
			kind := s.reg.StorageKind()
			m.repos.Set(float64(t.Repos))
			m.revisions.Set(float64(t.Revisions))
			m.blobs.With(kind).Set(float64(t.Blobs))
			m.blobBytes.With(kind).Set(float64(t.BlobBytes))
			m.pendingFiles.Set(float64(t.PendingFiles))
			m.pendingBytes.Set(float64(t.PendingBytes))
		}
		if follows, err := s.reg.Follows(ctx); err != nil {
			s.log.Warn("metrics: could not read the follow list", "error", err)
		} else {
			m.followed.Set(float64(len(follows)))
			m.oldestSync = time.Time{}
			for _, f := range follows {
				last := f.LastSuccess
				if last.IsZero() {
					last = f.CreatedAt
				}
				if m.oldestSync.IsZero() || last.Before(m.oldestSync) {
					m.oldestSync = last
				}
			}
		}
		if !m.storedKnown {
			if list, err := s.reg.Backups(ctx); err != nil {
				s.log.Warn("metrics: could not list the stored backups", "error", err)
			} else {
				m.storedKnown = true
				if len(list) > 0 {
					m.stored = list[len(list)-1]
				}
			}
		}
	}

	if m.oldestSync.IsZero() {
		m.syncOldest.Set(0)
	} else {
		m.syncOldest.Set(max(0, now.Sub(m.oldestSync).Seconds()))
	}

	a := s.reg.Activity()
	last, size := m.stored.ModTime, m.stored.Size
	if !a.LastBackup.IsZero() {
		last, size = a.LastBackup, a.LastBackupBytes
	}
	m.backupLast.Set(unixSeconds(last))
	m.backupSize.Set(float64(size))
	m.backups.With(outcomeSucceeded).Follow(a.BackupsStored)
	m.backups.With(outcomeFailed).Follow(a.BackupsFailed)
	m.backupPruneFailures.Follow(a.BackupPruneFailures)
	m.verifyLast.Set(unixSeconds(a.LastVerify))
	for _, status := range []string{registry.BlobOK, registry.BlobMissing, registry.BlobDamaged, registry.BlobUnreadable} {
		m.verifyFound.With(status).Set(float64(a.LastVerifyFound[status]))
	}
	m.verifyRuns.With(outcomeSucceeded).Follow(a.VerifyRuns)
	m.verifyRuns.With(outcomeFailed).Follow(a.VerifyRunsFailed)
}
