// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// countingTx passes a step's reads to the database under one context and
// counts its writes, to see what a step does when a read fails.
type countingTx struct {
	db      *sql.DB
	readCtx context.Context
	writes  int
}

func (c *countingTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	c.writes++
	return c.db.ExecContext(ctx, q, args...)
}

func (c *countingTx) QueryContext(_ context.Context, q string, args ...any) (*sql.Rows, error) {
	return c.db.QueryContext(c.readCtx, q, args...)
}

func (c *countingTx) QueryRowContext(_ context.Context, q string, args ...any) *sql.Row {
	return c.db.QueryRowContext(c.readCtx, q, args...)
}

// Moving a branch first looks up what the name is now. A failure of that
// lookup used to be ignored, and the branch written anyway.
func TestMoveBranchStopsWhenTheLookupFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	c, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Files: []File{file(t, r, "a", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	tx := &countingTx{db: r.db, readCtx: cancelled}
	err = moveBranch(ctx, tx, c.RepoID, "v1", c.SHA, r.stamp())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("moveBranch with a failing lookup = %v, want the lookup's error", err)
	}
	if tx.writes != 0 {
		t.Fatalf("moveBranch wrote %d times after its lookup failed", tx.writes)
	}

	// With the lookup working, the same call is refused for the right reason.
	tx = &countingTx{db: r.db, readCtx: ctx}
	if err := moveBranch(ctx, tx, c.RepoID, "v1", c.SHA, r.stamp()); !errors.Is(err, ErrConflict) || tx.writes != 0 {
		t.Fatalf("moving a tag = %v after %d writes, want ErrConflict and none", err, tx.writes)
	}
	// And a branch moves, and an empty name is no branch at all.
	if err := moveBranch(ctx, tx, c.RepoID, "dev", c.SHA, r.stamp()); err != nil || tx.writes != 1 {
		t.Fatalf("creating a branch = %v after %d writes", err, tx.writes)
	}
	if err := moveBranch(ctx, tx, c.RepoID, "", c.SHA, r.stamp()); err != nil || tx.writes != 1 {
		t.Fatalf("no branch = %v after %d writes", err, tx.writes)
	}
	if got, err := r.Resolve(ctx, "acme/model", "dev"); err != nil || got.SHA != c.SHA {
		t.Fatalf("dev = %v, %v", got.SHA, err)
	}
}

// lazy records upstreamSHA in acme/model with two pending files and
// returns the revision and the stored content for each.
func lazy(t *testing.T, r *Registry) (Commit, []File) {
	t.Helper()
	weights, config := put(t, r, "weights"), put(t, r, "config")
	c, err := r.CommitLazy(context.Background(), LazyInput{Repo: "acme/model", SHA: upstreamSHA, Files: []File{
		{Path: "model.bin", Size: weights.Size, SHA256: weights.SHA256, GitOID: strings.Repeat("a", 40), LFS: true},
		{Path: "config.json", Size: config.Size, GitOID: config.GitOID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return c, []File{{Path: "model.bin", SHA256: weights.SHA256}, {Path: "config.json", SHA256: config.SHA256}}
}

func TestCompleteRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("fulfils every file and moves the branch", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		c, stored := lazy(t, r)
		got, err := r.CompleteRevision(ctx, CompleteInput{Repo: "acme/model", SHA: upstreamSHA, Branch: "main",
			Upstream: "https://hub.example", Files: stored})
		if err != nil || got.SHA != upstreamSHA || got.ID != c.ID {
			t.Fatalf("complete = %+v, %v", got, err)
		}
		if n, _ := r.PendingCount(ctx, c); n != 0 {
			t.Fatalf("%d files still pending", n)
		}
		if head, err := r.Resolve(ctx, "acme/model", "main"); err != nil || head.SHA != upstreamSHA {
			t.Fatalf("main = %v, %v", head.SHA, err)
		}
		if repo, _ := r.GetRepo(ctx, "acme/model"); repo.Upstream != "https://hub.example" {
			t.Fatalf("upstream = %q", repo.Upstream)
		}
		// Complete means sealed: the revision can be built on, and recording
		// the same content under the same id again is the same revision.
		files, _ := r.Files(ctx, c)
		if again, err := r.Commit(ctx, CommitInput{Repo: "acme/model", SHA: upstreamSHA, Source: SourceMirror, Files: files}); err != nil || again.ID != c.ID {
			t.Fatalf("recording the completed revision again = %+v, %v", again, err)
		}
		// Completing what is complete only moves the branch.
		if _, err := r.CompleteRevision(ctx, CompleteInput{Repo: "acme/model", SHA: upstreamSHA, Branch: "stable", Files: stored}); err != nil {
			t.Fatalf("completing twice: %v", err)
		}
		if head, err := r.Resolve(ctx, "acme/model", "stable"); err != nil || head.SHA != upstreamSHA {
			t.Fatalf("stable = %v, %v", head.SHA, err)
		}
	})

	t.Run("a revision that is not recorded is reported as such", func(t *testing.T) {
		t.Parallel()
		r := open(t)
		_, stored := lazy(t, r)
		for name, in := range map[string]CompleteInput{
			"no such repository": {Repo: "acme/absent", SHA: upstreamSHA, Files: stored},
			"no such revision":   {Repo: "acme/model", SHA: strings.Repeat("f", 40), Files: stored},
		} {
			if _, err := r.CompleteRevision(ctx, in); !errors.Is(err, ErrRevisionNotFound) {
				t.Errorf("%s: %v, want ErrRevisionNotFound", name, err)
			}
		}
		for name, in := range map[string]CompleteInput{
			"bad repository name": {Repo: "model", SHA: upstreamSHA},
			"bad revision id":     {Repo: "acme/model", SHA: "main"},
			"bad branch name":     {Repo: "acme/model", SHA: upstreamSHA, Branch: "a..b"},
		} {
			if _, err := r.CompleteRevision(ctx, in); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: %v, want ErrInvalid", name, err)
			}
		}
	})

	// One transaction: a failure on any file, or on the branch, leaves the
	// revision exactly as pending as it was and the branch where it was.
	failures := []struct {
		name string
		in   func(stored []File) CompleteInput
		want error
	}{
		{"content that was never stored", func(stored []File) CompleteInput {
			stored[1].SHA256 = strings.Repeat("0", 64)
			return CompleteInput{Repo: "acme/model", SHA: upstreamSHA, Branch: "main", Files: stored}
		}, ErrBlobMissing},
		{"files left pending", func(stored []File) CompleteInput {
			return CompleteInput{Repo: "acme/model", SHA: upstreamSHA, Branch: "main", Files: stored[:1]}
		}, ErrConflict},
		{"a branch name that is a tag", func(stored []File) CompleteInput {
			return CompleteInput{Repo: "acme/model", SHA: upstreamSHA, Branch: "v1", Files: stored}
		}, ErrConflict},
	}
	for _, tc := range failures {
		t.Run("nothing changes on "+tc.name, func(t *testing.T) {
			t.Parallel()
			r := open(t)
			older, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Files: []File{file(t, r, "old", "old")}})
			if err != nil {
				t.Fatal(err)
			}
			c, stored := lazy(t, r)
			if _, err := r.CompleteRevision(ctx, tc.in(stored)); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if n, _ := r.PendingCount(ctx, c); n != 2 {
				t.Fatalf("%d files pending after the failure, want both: part of it was kept", n)
			}
			for _, ref := range []string{"main", "v1"} {
				if head, err := r.Resolve(ctx, "acme/model", ref); err != nil || head.SHA != older.SHA {
					t.Fatalf("%s = %v, %v; want it left at %s", ref, head.SHA, err, older.SHA)
				}
			}
		})
	}
}
