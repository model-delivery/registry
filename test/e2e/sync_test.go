// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// TestHubClientAfterSync: mirror a repository and follow it, move the
// upstream's branch, sync, and then the stock client gets the new content
// at main and the old content at the old revision id, with the upstream
// gone.
func TestHubClientAfterSync(t *testing.T) {
	python(t)
	ctx := context.Background()
	before := map[string]string{
		"config.json":         `{"model_type":"tiny"}`,
		"model.safetensors":   strings.Repeat("\x00weights\xff", 200_000),
		"tokenizer/vocab.txt": "a\nb\nc\n",
	}
	after := map[string]string{
		"config.json":         `{"model_type":"tiny","hidden_size":64}`,
		"model.safetensors":   strings.Repeat("\x00better weights\xff", 150_000),
		"tokenizer/vocab.txt": "a\nb\nc\n",
	}
	hub := regtest.Start(t, server.Config{AnonymousRead: true})
	hubClient := client.New(hub.URL, hub.Write)
	first, err := hubClient.Push(ctx, regtest.WriteTree(t, before), "acme/tiny", client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}

	local := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: hub.URL})
	cl := client.New(local.URL, local.Write)
	if _, err := cl.Mirror(ctx, "acme/tiny", "main", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Follow(ctx, "acme/tiny", client.FollowOptions{Branch: "main"}); err != nil {
		t.Fatal(err)
	}

	second, err := hubClient.Push(ctx, regtest.WriteTree(t, after), "acme/tiny", client.PushOptions{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := cl.Sync(ctx, "acme/tiny")
	if err != nil || !res.Changed || len(res.Refs) != 1 || res.Refs[0].SHA != second.SHA {
		t.Fatalf("sync: %+v, %v", res, err)
	}
	hub.StopServing() // the upstream is gone

	t.Run("main is the new revision", func(t *testing.T) {
		got := hfDownload(t, local.URL, "acme/tiny", "main", "config.json")
		checkSnapshot(t, local, "acme/tiny", second.SHA, got)
		sameContent(t, got.Snapshot, after)
	})
	t.Run("the old revision id still serves the old content", func(t *testing.T) {
		got := hfDownload(t, local.URL, "acme/tiny", first.SHA, "config.json")
		checkSnapshot(t, local, "acme/tiny", first.SHA, got)
		sameContent(t, got.Snapshot, before)
	})
}

// sameContent checks that the snapshot folder holds exactly files.
func sameContent(t *testing.T, snapshot string, files map[string]string) {
	t.Helper()
	for p, want := range files {
		got, err := os.ReadFile(snapshot + "/" + p)
		if err != nil || string(got) != want {
			t.Errorf("%s in %s: %d bytes, %v; want the %d bytes of that revision", p, snapshot, len(got), err, len(want))
		}
	}
}

// TestMirrorAllRefsFromHuggingFace mirrors every branch and tag of one
// small real repository from huggingface.co and compares what the
// registry recorded with what the hub's own refs API reports. The default
// is a dataset with two branches; REGISTRY_E2E_REFS_REPO names another, by
// key (datasets/namespace/name for a dataset).
func TestMirrorAllRefsFromHuggingFace(t *testing.T) {
	needsNetwork(t)
	ctx := context.Background()
	key := os.Getenv("REGISTRY_E2E_REFS_REPO")
	if key == "" {
		key = "datasets/" + flatDataset
	}
	typ, name := registry.SplitRepoKey(key)
	api := "https://huggingface.co/api/models/" + name + "/refs"
	if typ == registry.TypeDataset {
		api = "https://huggingface.co/api/datasets/" + name + "/refs"
	}
	resp, err := http.Get(api) //nolint:gosec,noctx // a fixed host; the test's own request.
	if err != nil {
		t.Fatal(err)
	}
	type ref struct {
		Name         string `json:"name"`
		TargetCommit string `json:"targetCommit"`
	}
	var hubRefs struct {
		Branches []ref `json:"branches"`
		Tags     []ref `json:"tags"`
	}
	err = json.NewDecoder(resp.Body).Decode(&hubRefs)
	_ = resp.Body.Close()
	if err != nil || len(hubRefs.Branches) == 0 {
		t.Fatalf("could not read the hub's refs: %v (%+v)", err, hubRefs)
	}

	local := regtest.Start(t, server.Config{AnonymousRead: true})
	done, err := client.New(local.URL, local.Write).MirrorAllRefs(ctx, key, os.Getenv("HF_TOKEN"), func(e client.MirrorEvent) {
		if e.Event == client.EventRef {
			t.Logf("%-8s %-6s %s  %s", e.Outcome, e.Kind, e.Ref, e.SHA)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range done.Refs {
		if r.Outcome != client.RefMirrored {
			t.Errorf("%s %s: %s %s", r.Kind, r.Name, r.Outcome, r.Detail)
		}
	}

	want := map[string]registry.Ref{}
	for _, b := range hubRefs.Branches {
		want[b.Name] = registry.Ref{Name: b.Name, Kind: registry.KindBranch, SHA: b.TargetCommit}
	}
	for _, tg := range hubRefs.Tags {
		want[tg.Name] = registry.Ref{Name: tg.Name, Kind: registry.KindTag, SHA: tg.TargetCommit}
	}
	repo, err := local.Reg.GetRepo(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := local.Reg.Refs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Errorf("the registry has %d refs, the hub reports %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		w, ok := want[r.Name]
		if !ok || w.Kind != r.Kind || w.SHA != r.SHA {
			t.Errorf("%s: the registry has a %s at %s; the hub reports %+v (listed: %v)", r.Name, r.Kind, r.SHA, w, ok)
		}
	}
	st, _ := local.Reg.Stats(ctx)
	t.Logf("%s: %d branches and %d tags, %d revisions, %d files stored (%d bytes)",
		key, len(hubRefs.Branches), len(hubRefs.Tags), st.Revisions, st.Blobs, st.StoredBytes)
}
