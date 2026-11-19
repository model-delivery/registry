// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"sync"
	"time"

	"github.com/model-delivery/registry/internal/blob"
)

// What the registry can say about itself for monitoring: how much it
// holds, and how its own maintenance (backups, verify runs) has gone since
// the process started. The server turns these into metrics; nothing here
// knows what a metric is.

// Totals is how much the registry holds, counted in one statement.
type Totals struct {
	Repos     int
	Revisions int
	// Blobs and BlobBytes are what is stored: each distinct content once.
	Blobs     int
	BlobBytes int64
	// PendingFiles and PendingBytes are the files revisions list whose
	// content has not been fetched from the upstream.
	PendingFiles int
	PendingBytes int64
}

// Totals counts what the registry holds. It reads the database only, never
// the storage backend, so its cost does not depend on where blobs are kept.
// It does read every row of the tables it counts, so a caller that asks
// often keeps the answer for a while.
func (r *Registry) Totals(ctx context.Context) (Totals, error) {
	var t Totals
	err := r.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM repos),
		(SELECT COUNT(*) FROM commits),
		b.n, b.bytes, p.n, p.bytes
		FROM (SELECT COUNT(*) AS n, COALESCE(SUM(size), 0) AS bytes FROM blobs) b,
		     (SELECT COUNT(*) AS n, COALESCE(SUM(size), 0) AS bytes FROM pending_files) p`).
		Scan(&t.Repos, &t.Revisions, &t.Blobs, &t.BlobBytes, &t.PendingFiles, &t.PendingBytes)
	return t, err
}

// StorageKind names the backend that holds the blobs (blob.Kind).
func (r *Registry) StorageKind() string { return blob.Kind(r.blobs) }

// Activity is what the registry's own maintenance has done since the
// process started. It is kept in memory: after a restart the counts start
// from zero and the times are zero until the next backup or verify run.
type Activity struct {
	// BackupsStored and BackupsFailed count calls to Backup by whether a
	// backup was stored. BackupPruneFailures counts backups that were stored
	// and could not be followed by the removal of old ones.
	BackupsStored       uint64
	BackupsFailed       uint64
	BackupPruneFailures uint64
	// LastBackup is when the last backup was stored, and LastBackupBytes its
	// size; zero when none has been since the process started.
	LastBackup      time.Time
	LastBackupBytes int64

	// VerifyRuns counts Verify runs that got through every blob, and
	// VerifyRunsFailed those that stopped early.
	VerifyRuns       uint64
	VerifyRunsFailed uint64
	// LastVerify is when the last run that got through every blob finished,
	// and LastVerifyFound how many blobs it found in each state (BlobOK,
	// BlobMissing, BlobDamaged, BlobUnreadable).
	LastVerify      time.Time
	LastVerifyFound map[string]int
}

// activity is the Activity being kept.
type activity struct {
	mu sync.Mutex
	a  Activity
}

// Activity returns what maintenance has done since the process started.
func (r *Registry) Activity() Activity {
	r.activity.mu.Lock()
	defer r.activity.mu.Unlock()
	out := r.activity.a
	out.LastVerifyFound = make(map[string]int, len(r.activity.a.LastVerifyFound))
	for status, n := range r.activity.a.LastVerifyFound {
		out.LastVerifyFound[status] = n
	}
	return out
}

// noteBackup records how a call to Backup ended.
func (r *Registry) noteBackup(res BackupResult, err error) {
	r.activity.mu.Lock()
	defer r.activity.mu.Unlock()
	a := &r.activity.a
	if err != nil {
		a.BackupsFailed++
		return
	}
	a.BackupsStored++
	a.LastBackup, a.LastBackupBytes = res.Backup.ModTime, res.Backup.Size
	if res.PruneErr != nil {
		a.BackupPruneFailures++
	}
}

// noteVerify records how a Verify run ended: found is how many blobs it
// found in each state, and err why it stopped early, if it did.
func (r *Registry) noteVerify(found map[string]int, err error) {
	at := r.now().UTC()
	r.activity.mu.Lock()
	defer r.activity.mu.Unlock()
	a := &r.activity.a
	if err != nil {
		a.VerifyRunsFailed++
		return
	}
	a.VerifyRuns++
	a.LastVerify, a.LastVerifyFound = at, found
}
