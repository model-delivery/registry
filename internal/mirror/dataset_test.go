// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
)

// A dataset is asked for at the hub's dataset addresses, recorded as a
// dataset under the hub's canonical name, and its files fetched from
// /datasets/{id}/resolve/. Nothing is asked of the model of the same name.
func TestMirrorADataset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	files := map[string]string{"data/train.csv": "a,b\n1,2\n", "README.md": "# card"}
	var mu sync.Mutex
	var asked []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/api/datasets/acme/old-name/revision/main":
			// Renamed upstream: the canonical id is what is stored.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "acme/table", "sha": hubSHA, "cardData": map[string]any{"license": "mit"}})
		case r.URL.Path == "/api/datasets/acme/table/tree/"+hubSHA:
			var out []map[string]any
			for p, c := range files {
				out = append(out, map[string]any{"type": "file", "path": p, "size": len(c), "oid": gitBlobID(c)})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/datasets/acme/table/resolve/"+hubSHA+"/"):
			c, ok := files[strings.TrimPrefix(r.URL.Path, "/datasets/acme/table/resolve/"+hubSHA+"/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(c))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	up := &Upstream{Endpoint: h.URL}
	reg := openRegistry(t)

	plan, err := Prepare(ctx, up, "datasets/acme/old-name", "main")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Repo != "datasets/acme/table" || plan.Type != registry.TypeDataset || plan.Info.ID != "acme/table" {
		t.Fatalf("plan for %q, type %q, upstream id %q", plan.Repo, plan.Type, plan.Info.ID)
	}
	var events []Event
	res, err := plan.Execute(ctx, reg, 1, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Repo != "datasets/acme/table" || res.Files != 2 {
		t.Fatalf("result %+v", res)
	}
	start, done := events[0], events[len(events)-1]
	if start.Repo != "acme/table" || start.RepoType != registry.TypeDataset || done.Repo != "acme/table" || done.RepoType != registry.TypeDataset {
		t.Fatalf("start %+v, done %+v", start, done)
	}
	if line, _ := json.Marshal(done); !strings.Contains(string(line), `"repo":"acme/table","repo_type":"dataset"`) {
		t.Fatalf("the done line reads %s", line)
	}

	c, err := reg.Resolve(ctx, "datasets/acme/table", "main")
	if err != nil || c.RepoType != registry.TypeDataset || c.SHA != hubSHA || !strings.Contains(c.UpstreamInfo, "mit") {
		t.Fatalf("the recorded dataset: %+v, %v", c, err)
	}
	if _, err := reg.GetRepo(ctx, "acme/table"); !errors.Is(err, registry.ErrRepoNotFound) {
		t.Fatalf("a model was recorded too: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range asked {
		if strings.Contains(p, "/api/models/") || (strings.Contains(p, "/resolve/") && !strings.HasPrefix(p, "/datasets/")) {
			t.Errorf("the model addresses were asked: %s", p)
		}
	}
}

// A model's events are what they were before datasets existed: no type.
func TestModelEventsCarryNoType(t *testing.T) {
	t.Parallel()
	h := newHub(t, map[string]string{"config.json": "{}"})
	plan, err := Prepare(context.Background(), &Upstream{Endpoint: h.URL}, "acme/tiny", "main")
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if _, err := plan.Execute(context.Background(), openRegistry(t), 1, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.RepoType != "" {
			t.Errorf("a model's %s event has type %q", e.Event, e.RepoType)
		}
	}
}

// A dataset the hub does not have is "not found upstream", even when the
// hub has a model of that name.
func TestMirrorADatasetTheHubDoesNotHave(t *testing.T) {
	t.Parallel()
	h := newHub(t, map[string]string{"config.json": "{}"})
	_, err := Prepare(context.Background(), &Upstream{Endpoint: h.URL}, "datasets/acme/tiny", "main")
	if !errors.Is(err, ErrUpstreamNotFound) || !FromUpstream(err) {
		t.Fatalf("Prepare of a dataset only a model has: %v", err)
	}
	if _, err := Prepare(context.Background(), &Upstream{Endpoint: h.URL}, "datasets/acme/a/b", "main"); !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("Prepare of a malformed key: %v", err)
	}
}
