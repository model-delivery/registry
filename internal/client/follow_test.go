// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Following, unfollowing and syncing address a repository by its key, a
// model under /api/v1/repos and a dataset under /api/v1/datasets, and give
// back the registry's document as a type.
func TestFollowAndSyncMethods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	when := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ key, path string }{
		{"acme/tiny", "/api/v1/repos/acme/tiny"},
		{"datasets/acme/tiny", "/api/v1/datasets/acme/tiny"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			const entry = `{"name":"acme/tiny","branch":"main","created_at":"2026-11-18T10:00:00Z","last_check":"2026-11-18T10:00:00Z","failures":2,"last_error":"upstream returned 503"}`
			cl, got := answering(t, http.StatusOK, entry)
			f, err := cl.Follow(ctx, tc.key, FollowOptions{Branch: "main"})
			if err != nil || f.Branch != "main" || !f.LastCheck.Equal(when) || f.Failures != 2 || f.LastError == "" || string(f.Raw) != entry {
				t.Fatalf("Follow: %+v, %v", f, err)
			}
			if _, err := cl.Follow(ctx, tc.key, FollowOptions{AllRefs: true}); err != nil {
				t.Fatal(err)
			}
			if _, err := cl.Follow(ctx, tc.key, FollowOptions{}); err != nil {
				t.Fatal(err)
			}
			r := got()
			if len(r) != 3 || r[0].method != http.MethodPost || r[0].path != tc.path+"/follow" ||
				r[0].body != `{"branch":"main"}` || r[1].body != `{"all_refs":true}` || r[2].body != `{}` {
				t.Fatalf("follow requests: %+v", r)
			}

			const synced = `{"name":"acme/tiny","checked_at":"2026-11-18T10:00:00Z","changed":true,"downloaded_bytes":9,"refs":[{"name":"main","kind":"branch","outcome":"mirrored","sha":"bb","previous":"aa"}]}`
			cl, got = answering(t, http.StatusOK, synced)
			s, err := cl.Sync(ctx, tc.key)
			if err != nil || !s.Changed || len(s.Refs) != 1 || s.Refs[0] != (RefResult{Name: "main", Kind: "branch", Outcome: RefMirrored, SHA: "bb", Previous: "aa"}) {
				t.Fatalf("Sync: %+v, %v", s, err)
			}
			if r := got(); len(r) != 1 || r[0].method != http.MethodPost || r[0].path != tc.path+"/sync" {
				t.Fatalf("sync requests: %+v", r)
			}

			cl, got = answering(t, http.StatusNoContent, "")
			if err := cl.Unfollow(ctx, tc.key); err != nil {
				t.Fatal(err)
			}
			if r := got(); len(r) != 1 || r[0].method != http.MethodDelete || r[0].path != tc.path+"/follow" {
				t.Fatalf("unfollow requests: %+v", r)
			}
		})
	}
}

func TestFollowListAndSyncAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const list = `{"follows":[{"name":"acme/tiny","type":"dataset","all_refs":true,"created_at":"2026-11-18T10:00:00Z"}],"interval_seconds":3600}`
	cl, got := answering(t, http.StatusOK, list)
	l, err := cl.ListFollows(ctx)
	if err != nil || len(l.Follows) != 1 || l.Follows[0].Key() != "datasets/acme/tiny" || !l.Follows[0].AllRefs || l.IntervalSeconds != 3600 || string(l.Raw) != list {
		t.Fatalf("ListFollows: %+v, %v", l, err)
	}
	if r := got(); r[0].method != http.MethodGet || r[0].path != "/api/v1/follows" {
		t.Fatalf("requests: %+v", r)
	}

	cl, got = answering(t, http.StatusAccepted, `{"queued":["acme/tiny","datasets/acme/x"]}`)
	q, err := cl.SyncAll(ctx)
	if err != nil || len(q.Queued) != 2 {
		t.Fatalf("SyncAll: %+v, %v", q, err)
	}
	if r := got(); r[0].method != http.MethodPost || r[0].path != "/api/v1/sync" {
		t.Fatalf("requests: %+v", r)
	}

	// A check that failed is an error carrying the registry's reason.
	cl, _ = answering(t, http.StatusBadGateway, `{"name":"acme/tiny","refs":[],"error":"main: upstream returned 503"}`)
	var apiErr *APIError
	if _, err := cl.Sync(ctx, "acme/tiny"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadGateway || apiErr.Message != "main: upstream returned 503" {
		t.Fatalf("a failed sync: %v", err)
	}
}

// A mirror of every ref asks for all_refs, names no revision, and returns
// the done line with every ref's result.
func TestMirrorAllRefs(t *testing.T) {
	t.Parallel()
	const stream = `{"event":"ref","ref":"main","kind":"branch","outcome":"mirrored","sha":"aa"}
{"event":"ref","ref":"v1","kind":"tag","outcome":"tag_moved","sha":"bb","previous":"aa","detail":"moved"}
{"event":"done","repo":"acme/tiny","repo_type":"dataset","downloaded_bytes":5,"refs":[{"name":"main","kind":"branch","outcome":"mirrored","sha":"aa"},{"name":"v1","kind":"tag","outcome":"tag_moved","sha":"bb","previous":"aa","detail":"moved"}]}
`
	cl, got := answering(t, http.StatusOK, stream)
	var lines []MirrorEvent
	done, err := cl.MirrorAllRefs(context.Background(), "datasets/acme/tiny", "", func(e MirrorEvent) { lines = append(lines, e) })
	if err != nil || len(done.Refs) != 2 || done.Refs[1].Outcome != RefTagMoved || done.RepoType != Dataset {
		t.Fatalf("done %+v, %v", done, err)
	}
	if len(lines) != 2 || lines[1].Event != EventRef || lines[1].Ref != "v1" || lines[1].Outcome != RefTagMoved || lines[1].Previous != "aa" {
		t.Fatalf("lines %+v", lines)
	}
	if r := got(); len(r) != 1 || r[0].body != `{"all_refs":true,"repo":"acme/tiny","type":"dataset","upstream_token":""}` {
		t.Fatalf("request %+v", r)
	}
}
