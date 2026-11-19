// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// A request is counted under a class of route, which is one of a fixed few
// names whatever the request's path holds.
func TestRouteClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/healthz", routeProbe},
		{http.MethodGet, "/metrics", routeProbe},

		{http.MethodGet, "/acme/tiny/resolve/main/config.json", routeDownload},
		{http.MethodHead, "/acme/tiny/resolve/main/sub/dir/model.bin", routeDownload},
		{http.MethodGet, "/acme/tiny/resolve/refs%2Fpr%2F1/x.txt", routeDownload},
		// A repository named like a route is still a repository.
		{http.MethodGet, "/metrics/tiny/resolve/main/config.json", routeDownload},

		{http.MethodPut, "/api/v1/blobs/" + strings.Repeat("a", 64), routeUpload},
		{http.MethodPut, "/api/lfs/upload/" + strings.Repeat("a", 64), routeUpload},
		{http.MethodPost, "/api/models/acme/tiny/commit/main", routeUpload},

		{http.MethodGet, "/api/v1/repos", routeAPI},
		{http.MethodGet, "/api/v1/repos/acme/tiny/revisions/main", routeAPI},
		{http.MethodHead, "/api/v1/blobs/" + strings.Repeat("a", 64), routeAPI},
		{http.MethodPost, "/api/v1/repos/acme/tiny/commits", routeAPI},
		{http.MethodDelete, "/api/v1/repos/acme/tiny", routeAPI},
		{http.MethodPost, "/api/v1/gc", routeAPI},
		{http.MethodPost, "/api/v1/backups", routeAPI},
		{http.MethodGet, "/api/v1/nothing-here", routeAPI},

		{http.MethodPost, "/api/v1/mirror", routeTask},
		{http.MethodPost, "/api/v1/verify", routeTask},
		{http.MethodGet, "/api/v1/backups/snapshot", routeTask},
		{http.MethodGet, "/api/v1/backup/snapshot", routeTask},

		{http.MethodGet, "/api/models/acme/tiny", routeHF},
		{http.MethodGet, "/api/models/acme/tiny/tree/main", routeHF},
		{http.MethodPost, "/api/models/acme/tiny/preupload/main", routeHF},
		{http.MethodGet, "/api/whoami-v2", routeHF},
		{http.MethodPost, "/api/repos/create", routeHF},
		{http.MethodPost, "/acme/tiny.git/info/lfs/objects/batch", routeHF},
		{http.MethodGet, "/api/something-else", routeHF},

		{http.MethodGet, "/", routeUI},
		{http.MethodGet, "/ui/repo/acme/tiny", routeUI},
		{http.MethodPost, "/ui/login", routeUI},
		{http.MethodGet, "/static/app.css", routeUI},
		{http.MethodGet, "/acme/tiny", routeUI},

		{http.MethodGet, "/favicon.ico", routeOther},
		{http.MethodGet, "/acme/tiny/blob/main/config.json", routeOther},
		{http.MethodGet, "/a//b", routeOther},
		{http.MethodGet, "/acme/tiny/resolve/main/%00", routeDownload},
		{"PROPFIND", "/acme/tiny/resolve/main/config.json", routeDownload},

		// Datasets, at the Hub's dataset addresses.
		{http.MethodGet, "/datasets/acme/tiny/resolve/main/data/train.csv", routeDownload},
		{http.MethodPost, "/api/datasets/acme/tiny/commit/main", routeUpload},
		{http.MethodGet, "/api/v1/datasets", routeAPI},
		{http.MethodDelete, "/api/v1/datasets/acme/tiny", routeAPI},
		{http.MethodGet, "/api/datasets/acme/tiny/tree/main", routeHF},
		{http.MethodPost, "/api/datasets/acme/tiny/paths-info/main", routeHF},
		{http.MethodPost, "/datasets/acme/tiny.git/info/lfs/objects/batch", routeHF},
		{http.MethodGet, "/ui/dataset/acme/tiny", routeUI},
		{http.MethodGet, "/datasets/acme/tiny", routeUI},
		// A model in the datasets namespace, recorded before it was reserved.
		{http.MethodGet, "/datasets/legacy/resolve/main/config.json", routeDownload},
		{http.MethodGet, "/datasets/legacy", routeUI},
		{http.MethodGet, "/info?dataset=acme/tiny", routeOther},
	}
	classes := []string{routeDownload, routeUpload, routeAPI, routeTask, routeHF, routeUI, routeProbe, routeOther}
	for _, tc := range cases {
		// As the server parses a request line.
		u, err := url.ParseRequestURI(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		got := routeClass(&http.Request{Method: tc.method, URL: u})
		if got != tc.want {
			t.Errorf("%s %s is counted as %q, want %q", tc.method, tc.path, got, tc.want)
		}
		if !slices.Contains(classes, got) {
			t.Errorf("%s %s: %q is not a class of route", tc.method, tc.path, got)
		}
	}
	for method, want := range map[string]string{"GET": "GET", "HEAD": "HEAD", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE",
		"PATCH": methodOther, "OPTIONS": methodOther, "PROPFIND": methodOther, "get": methodOther, "acme/tiny": methodOther, "": methodOther} {
		if got := methodLabel(method); got != want {
			t.Errorf("method %q is counted as %q, want %q", method, got, want)
		}
	}
	for status, want := range map[int]string{100: "1xx", 200: "2xx", 206: "2xx", 302: "3xx", 304: "3xx", 401: "4xx", 404: "4xx", 499: "4xx", 500: "5xx", 503: "5xx", 0: "5xx", 999: "5xx"} {
		if got := statusClass(status); got != want {
			t.Errorf("status %d is counted as %q, want %q", status, got, want)
		}
	}
}

// How a pull-through job ended, as its metric names it.
func TestPullOutcome(t *testing.T) {
	t.Parallel()
	wrapped := func(err error) error { return fmt.Errorf("model.bin: %w", err) }
	cases := []struct {
		name    string
		err     error
		listing bool
		want    string
	}{
		{"no error", nil, false, outcomeSucceeded},
		{"the server stopped", errShuttingDown, false, outcomeCancelled},
		{"a cancelled context", wrapped(context.Canceled), false, outcomeCancelled},
		{"content that does not match its digest", wrapped(blob.ErrMismatch), false, outcomeVerification},
		{"a file the upstream no longer has", wrapped(mirror.ErrUpstreamNotFound), false, outcomeUpstream},
		{"the upstream denies access", wrapped(mirror.ErrUpstreamAuth), false, outcomeUpstream},
		{"a redirect that is not followed", wrapped(mirror.ErrUnsafeUpstream), false, outcomeUpstream},
		{"a full disk", errors.New("write /srv/registry/tmp/stage-1: no space left on device"), false, outcomeLocal},
		{"the database", wrapped(errors.New("database is locked")), false, outcomeLocal},

		{"a listing recorded", nil, true, outcomeSucceeded},
		{"a repository the upstream does not have", wrapped(mirror.ErrUpstreamNotFound), true, outcomeNotFound},
		{"a repository that was created here", wrapped(registry.ErrLocalRepo), true, outcomeNotFound},
		{"a name the registry does not accept", wrapped(registry.ErrInvalid), true, outcomeNotFound},
		{"the upstream denies access to a listing", wrapped(mirror.ErrUpstreamAuth), true, outcomeUpstream},
		{"the database, for a listing", errors.New("database is locked"), true, outcomeLocal},
		{"the server stopped, for a listing", errShuttingDown, true, outcomeCancelled},
	}
	for _, tc := range cases {
		if got := pullOutcome(tc.err, tc.listing); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The writer a handler is given notes the status and is otherwise the
// server's own: a file is still sent with ReadFrom, a flush and a read
// deadline still reach the connection, and http.MaxBytesReader is still
// handed the writer that can close the connection after an oversized body.
func TestCountingDoesNotChangeTheWriter(t *testing.T) {
	t.Parallel()
	s := New(nil, Config{})
	t.Cleanup(s.stop)
	content := strings.Repeat("a file sent with ReadFrom. ", 4096)
	path := t.TempDir() + "/file.bin"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var inside http.ResponseWriter
	h := s.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inside = w
		switch r.URL.Path {
		case "/file":
			if _, ok := w.(io.ReaderFrom); !ok {
				t.Error("the writer has lost ReadFrom")
			}
			f, err := os.Open(path) //nolint:gosec // a file this test wrote.
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			counted := &countedWriter{ResponseWriter: w, sent: s.m.bytesFromStore}
			if _, err := io.Copy(counted, f); err != nil {
				t.Error(err)
			}
		case "/flush":
			rc := http.NewResponseController(w)
			_, _ = w.Write([]byte("first"))
			if err := rc.Flush(); err != nil {
				t.Errorf("flush: %v", err)
			}
			if err := rc.SetReadDeadline(time.Time{}); err != nil {
				t.Errorf("read deadline: %v", err)
			}
			_, _ = w.Write([]byte(" second"))
		case "/large":
			// More than the limit: the server must close the connection.
			_, err := io.Copy(io.Discard, http.MaxBytesReader(baseWriter(w), r.Body, 8))
			if !errors.As(err, new(*http.MaxBytesError)) {
				t.Errorf("reading an oversized body: %v", err)
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case "/nothing":
		}
	}))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	fetch := func(method, path string, body io.Reader) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(got)
	}

	if resp, got := fetch(http.MethodGet, "/file", nil); resp.StatusCode != http.StatusOK || got != content {
		t.Fatalf("the file arrived as %d bytes with status %d", len(got), resp.StatusCode)
	}
	if _, wrapped := inside.(*statusWriter); !wrapped {
		t.Fatalf("the handler was given a %T", inside)
	}
	if _, still := baseWriter(inside).(*statusWriter); still {
		t.Fatal("baseWriter returned the wrapper")
	}
	if got := s.m.bytesFromStore.Value(); got != uint64(len(content)) {
		t.Fatalf("%d bytes counted for a file of %d", got, len(content))
	}
	if _, got := fetch(http.MethodGet, "/flush", nil); got != "first second" {
		t.Fatalf("flushed response = %q", got)
	}
	if resp, _ := fetch(http.MethodPost, "/large", strings.NewReader(strings.Repeat("x", 1024))); resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Fatalf("after an oversized body: status %d, connection kept open: %v; the server closes it when MaxBytesReader is given its own writer",
			resp.StatusCode, !resp.Close)
	}
	if resp, _ := fetch(http.MethodGet, "/nothing", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("a handler that writes nothing: status %d", resp.StatusCode)
	}

	var out bytes.Buffer
	if err := s.m.set.Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`modelregistry_http_requests_total{route="other",method="GET",code="2xx"} 3`,
		`modelregistry_http_requests_total{route="other",method="POST",code="4xx"} 1`,
		`modelregistry_http_request_duration_seconds_count{route="other"} 4`,
		`modelregistry_http_requests_in_flight 0`,
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("missing %q in:\n%s", line, out.String())
		}
	}
}

// A handler that panics to abort its response is still counted, and is no
// longer in flight.
func TestAnAbortedResponseIsCounted(t *testing.T) {
	t.Parallel()
	s := New(nil, Config{})
	t.Cleanup(s.stop)
	h := s.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not pass through")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/acme/tiny/resolve/main/model.bin", nil))
	}()
	if got := s.m.requests.With(routeDownload, http.MethodGet, "2xx").Value(); got != 1 || s.m.inFlight.Value() != 0 {
		t.Fatalf("%d requests counted, %v in flight", got, s.m.inFlight.Value())
	}
}

// A Server made without New, as some tests make one, counts nothing and
// does not fail for it.
func TestAServerWithoutMetricsCountsNothing(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.uploaded(blob.Info{Size: 10})
	s.m.commits.With(sourcePush).Inc()
	s.m.pull.fetches.With(outcomeLocal).Inc()
	rec := httptest.NewRecorder()
	s.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status %d", rec.Code)
	}
}

// operationsDocument is docs/operations.md, from this package's directory.
const operationsDocument = "../../docs/operations.md"

// docs/operations.md lists every metric, and names none that does not
// exist. A metric added, renamed or removed fails here until the document
// follows.
func TestEveryMetricIsDocumented(t *testing.T) {
	t.Parallel()
	s := New(nil, Config{Version: "test"})
	t.Cleanup(s.stop)
	var out bytes.Buffer
	if err := s.m.set.Write(&out); err != nil {
		t.Fatal(err)
	}
	exposed := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^# TYPE (\S+) (\S+)$`).FindAllStringSubmatch(out.String(), -1) {
		exposed[m[1]] = m[2]
		if !strings.HasPrefix(m[1], "modelregistry_") {
			t.Errorf("%s does not carry the registry's prefix", m[1])
		}
		if (m[2] == "counter") != strings.HasSuffix(m[1], "_total") {
			t.Errorf("%s is a %s: counters, and only counters, are named ..._total", m[1], m[2])
		}
		if !strings.Contains(out.String(), "# HELP "+m[1]+" ") {
			t.Errorf("%s has no HELP line", m[1])
		}
	}
	if len(exposed) < 30 {
		t.Fatalf("found only %d metrics: this test no longer sees them", len(exposed))
	}

	raw, err := os.ReadFile(operationsDocument)
	if err != nil {
		t.Fatal(err)
	}
	// The table: a metric in backticks, then its type.
	row := regexp.MustCompile("(?m)^\\| `(modelregistry_[a-z0-9_]+)` \\| (counter|gauge|histogram) \\|")
	documented := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		documented[m[1]] = m[2]
	}
	for name, kind := range exposed {
		if documented[name] != kind {
			t.Errorf("%s is a %s; docs/operations.md lists it as %q (add a row to the table under Monitoring)", name, kind, documented[name])
		}
	}
	for name := range documented {
		if _, exists := exposed[name]; !exists {
			t.Errorf("docs/operations.md lists %s, which the registry does not have", name)
		}
	}
	// Whatever else the document says about a metric (an alert, an
	// expression) names one that exists.
	for _, name := range regexp.MustCompile(`modelregistry_[a-z0-9_]+`).FindAllString(string(raw), -1) {
		base := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if trimmed, ok := strings.CutSuffix(name, suffix); ok && exposed[trimmed] == "histogram" {
				base = trimmed
			}
		}
		if _, exists := exposed[base]; !exists {
			t.Errorf("docs/operations.md mentions %s, which the registry does not have", name)
		}
	}
}
