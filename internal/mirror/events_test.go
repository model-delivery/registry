// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
)

// The progress lines are what `POST /api/v1/mirror` streams and what
// clients parse, so the text of each is fixed.
func TestEventsKeepTheirWireValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		e    Event
		want string
	}{
		{Event{Event: EventStart, Repo: "acme/tiny", SHA: hubSHA, Files: 2, Bytes: 9},
			`{"event":"start","repo":"acme/tiny","sha":"` + hubSHA + `","files":2,"bytes":9}`},
		{Event{Event: EventFile, Path: "a.bin", Size: 4, Status: StatusDownloaded},
			`{"event":"file","path":"a.bin","status":"downloaded","size":4}`},
		{Event{Event: EventFile, Path: "b.bin", Size: 5, Status: StatusReused},
			`{"event":"file","path":"b.bin","status":"reused","size":5}`},
		{Result{Repo: "acme/tiny", SHA: hubSHA, Ref: "main", Files: 2, Bytes: 9, DownloadedBytes: 4, ReusedBytes: 5}.doneEvent(),
			`{"event":"done","repo":"acme/tiny","sha":"` + hubSHA + `","ref":"main","files":2,"bytes":9,"downloaded_bytes":4,"reused_bytes":5}`},
		{Event{Event: EventError, Error: "it failed"}, `{"event":"error","error":"it failed"}`},
	}
	for _, tc := range tests {
		got, err := json.Marshal(tc.e)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s event encodes as %s (%v), want %s", tc.e.Event, got, err, tc.want)
		}
	}
}

// Execute reports progress through its callback and what it did through
// its result. The last progress line of a success is "done"; a failure is
// the returned error and sends no closing line of its own.
func TestExecuteReportsProgressThenItsResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	files := map[string]string{"config.json": "{}", "weights.bin": "weights"}

	collect := func() (func(Event), func() []Event) {
		var mu sync.Mutex
		var seen []Event
		return func(e Event) {
				mu.Lock()
				seen = append(seen, e)
				mu.Unlock()
			}, func() []Event {
				mu.Lock()
				defer mu.Unlock()
				return append([]Event(nil), seen...)
			}
	}

	t.Run("success", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, files)
		h.lfs = map[string]bool{"weights.bin": true}
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		progress, events := collect()
		// Below 1 means the default, which is more than one at a time.
		res, err := plan.Execute(ctx, reg, 0, progress)
		if err != nil {
			t.Fatal(err)
		}
		total := int64(len("{}") + len("weights"))
		want := Result{Repo: "acme/tiny", SHA: hubSHA, Ref: "main", Files: 2, Bytes: total, DownloadedBytes: total}
		if res != want {
			t.Fatalf("result = %+v, want %+v", res, want)
		}
		got := events()
		if len(got) != 4 {
			t.Fatalf("%d events, want start, two files and done: %+v", len(got), got)
		}
		if got[0].Event != EventStart || got[0].Files != 2 || got[0].Bytes != total || got[0].SHA != hubSHA {
			t.Errorf("first event = %+v, want the start line", got[0])
		}
		paths := map[string]FileStatus{}
		for _, e := range got[1:3] {
			if e.Event != EventFile {
				t.Errorf("event %+v between start and done, want a file line", e)
			}
			paths[e.Path] = e.Status
		}
		if paths["config.json"] != StatusDownloaded || paths["weights.bin"] != StatusDownloaded {
			t.Errorf("file lines = %v", paths)
		}
		if got[3] != res.doneEvent() {
			t.Errorf("last event = %+v, want the done line for %+v", got[3], res)
		}

		// A second run fetches nothing and says so.
		progress, events = collect()
		again, err := plan.Execute(ctx, reg, 1, progress)
		if err != nil || again.ReusedBytes != total || again.DownloadedBytes != 0 {
			t.Fatalf("second run = %+v, %v", again, err)
		}
		for _, e := range events() {
			if e.Event == EventFile && e.Status != StatusReused {
				t.Errorf("second run: %+v, want it reused", e)
			}
		}
	})

	t.Run("failure", func(t *testing.T) {
		t.Parallel()
		h := newHub(t, files)
		h.wrap = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/weights.bin") && strings.Contains(r.URL.Path, "/resolve/") {
				_, _ = io.WriteString(w, "damage")
				return true
			}
			return false
		}
		reg := openRegistry(t)
		plan, err := Prepare(ctx, &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
		if err != nil {
			t.Fatal(err)
		}
		progress, events := collect()
		res, err := plan.Execute(ctx, reg, 1, progress)
		if !errors.Is(err, blob.ErrMismatch) || res != (Result{}) {
			t.Fatalf("result = %+v, %v; want no result and the verification failure", res, err)
		}
		for _, e := range events() {
			if e.Event == EventDone || e.Event == EventError {
				t.Errorf("a failed mirror sent %+v", e)
			}
		}
	})
}
