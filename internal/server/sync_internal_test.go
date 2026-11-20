// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/registry"
)

// A failing repository waits twice as long after each failure, up to a
// day, or the interval when that is longer.
func TestSyncWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{0, time.Hour, time.Hour},
		{1, time.Hour, 2 * time.Hour},
		{3, time.Hour, 8 * time.Hour},
		{5, time.Hour, 24 * time.Hour},
		{1000, time.Hour, 24 * time.Hour},
		{0, 48 * time.Hour, 48 * time.Hour},
		{4, 48 * time.Hour, 48 * time.Hour},
		{2, 5 * time.Minute, 20 * time.Minute},
	}
	for _, tc := range tests {
		if got := syncWait(tc.failures, tc.interval); got != tc.want {
			t.Errorf("%d failures, every %s: wait %s, want %s", tc.failures, tc.interval, got, tc.want)
		}
	}
}

// Each repository has its own place within the interval, the same every
// time, and repositories followed together are spread over it.
func TestSyncPhaseSpreadsRepositories(t *testing.T) {
	t.Parallel()
	const interval = time.Hour
	seen := map[time.Duration]bool{}
	for _, key := range []string{"acme/a", "acme/b", "acme/c", "acme/d", "datasets/acme/a", "hub/model"} {
		p := syncPhase(key, interval)
		if p < 0 || p >= interval || p != syncPhase(key, interval) {
			t.Fatalf("%s: phase %s", key, p)
		}
		seen[p] = true
	}
	if len(seen) < 5 {
		t.Fatalf("six repositories share %d places in the interval", len(seen))
	}
	if syncPhase("acme/a", 0) != 0 {
		t.Fatal("a phase without an interval")
	}
}

// When a repository is next due: not before its place in the first
// interval, then one wait after its last check.
func TestSyncDue(t *testing.T) {
	t.Parallel()
	started := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	f := registry.Follow{Name: "acme/tiny", Branch: "main"}
	first := started.Add(syncPhase("acme/tiny", time.Hour))
	if got := syncDue(f, time.Hour, started); !got.Equal(first) {
		t.Fatalf("never checked: due %s, want %s", got, first)
	}
	// Checked long before this process started: its place still holds.
	f.LastCheck = started.Add(-48 * time.Hour)
	if got := syncDue(f, time.Hour, started); !got.Equal(first) {
		t.Fatalf("overdue after a restart: due %s, want %s", got, first)
	}
	f.LastCheck = started.Add(2 * time.Hour)
	if got := syncDue(f, time.Hour, started); !got.Equal(started.Add(3 * time.Hour)) {
		t.Fatalf("checked: due %s", got)
	}
	f.Failures = 2
	if got := syncDue(f, time.Hour, started); !got.Equal(started.Add(6 * time.Hour)) {
		t.Fatalf("after two failures: due %s", got)
	}
}
