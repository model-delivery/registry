// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"

	"github.com/model-delivery/registry/internal/blob"
)

// Content is verified when it is stored. Verify asks the same question
// again of everything in storage: is each recorded blob still there, the
// right size, and the content its name says it is? It finds damage on the
// disk or in the bucket, and files changed by something other than the
// registry.

// What a check found.
const (
	BlobOK = "ok"
	// BlobMissing: the registry has a record of the blob and storage has
	// nothing under its name.
	BlobMissing = "missing"
	// BlobDamaged: the stored bytes have the wrong size or no longer hash
	// to the blob's name.
	BlobDamaged = "damaged"
	// BlobUnreadable: the blob could not be read, so nothing is known about
	// its content. An unreachable bucket shows up as this.
	BlobUnreadable = "unreadable"
)

// BlobCheck is the outcome of checking one stored blob.
type BlobCheck struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	// Detail says what is wrong, when something is.
	Detail string `json:"detail,omitempty"`
	// Repos names repositories with a revision that uses the blob, when
	// something is wrong with it: at most maxReposPerCheck of them, each by
	// its key (a dataset as "datasets/namespace/name").
	Repos []string `json:"repos,omitempty"`
}

// VerifyResult totals a Verify run.
type VerifyResult struct {
	Blobs  int   `json:"blobs"`
	Bytes  int64 `json:"bytes"`
	Failed int   `json:"failed"`
}

const (
	verifyPage       = 500
	maxReposPerCheck = 10
)

// BlobTotals is how many blobs are recorded, and their total size.
func (r *Registry) BlobTotals(ctx context.Context) (blobs int, bytes int64, err error) {
	err = r.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs").Scan(&blobs, &bytes)
	return blobs, bytes, err
}

// Verify re-reads every recorded blob and calls report with what it found,
// one blob at a time, in digest order. It returns early only if ctx ends
// or the database cannot be read; damage is reported, not returned.
//
// It is safe while the registry serves. It takes none of the registry's
// locks and keeps no database cursor open while it reads content: the
// blobs are listed a page at a time and each is then read through the
// storage backend like a download. A blob that is removed while the run is
// under way (by garbage collection or a delete) is passed over, not
// reported as missing.
func (r *Registry) Verify(ctx context.Context, report func(BlobCheck)) (VerifyResult, error) {
	found := map[string]int{}
	res, err := r.verify(ctx, func(c BlobCheck) {
		found[c.Status]++
		report(c)
	})
	r.noteVerify(found, err)
	return res, err
}

func (r *Registry) verify(ctx context.Context, report func(BlobCheck)) (VerifyResult, error) {
	var res VerifyResult
	after := ""
	for {
		page, err := r.blobsAfter(ctx, after, verifyPage)
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			return res, nil
		}
		for _, b := range page {
			after = b.SHA256
			check, counted, err := r.checkBlob(ctx, b)
			if err != nil {
				return res, err
			}
			if !counted {
				continue
			}
			res.Blobs++
			res.Bytes += b.Size
			if check.Status != BlobOK {
				res.Failed++
			}
			report(check)
		}
	}
}

// checkBlob checks one blob. counted is false when the blob stopped being
// recorded while it was being checked.
func (r *Registry) checkBlob(ctx context.Context, b blob.Info) (check BlobCheck, counted bool, err error) {
	check = BlobCheck{SHA256: b.SHA256, Size: b.Size, Status: BlobOK}
	found := blob.Check(ctx, r.blobs, b.SHA256, b.Size)
	if found == nil {
		return check, true, nil
	}
	if err := ctx.Err(); err != nil {
		return check, false, err
	}
	// Whatever went wrong, it only matters if the blob is still meant to
	// be there.
	if _, still, err := r.StatBlob(ctx, b.SHA256); err != nil {
		return check, false, err
	} else if !still {
		return check, false, nil
	}
	switch {
	case errors.Is(found, blob.ErrNotFound):
		check.Status, check.Detail = BlobMissing, "storage holds nothing under this name"
	case errors.Is(found, blob.ErrMismatch):
		check.Status, check.Detail = BlobDamaged, found.Error()
	default:
		check.Status, check.Detail = BlobUnreadable, found.Error()
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT CASE r.type WHEN ? THEN ? || r.name ELSE r.name END AS repo
		 FROM files f JOIN commits c ON c.id = f.commit_id JOIN repos r ON r.id = c.repo_id
		 WHERE f.sha256 = ? ORDER BY repo LIMIT ?`, TypeDataset, datasetKeyPrefix, b.SHA256, maxReposPerCheck)
	if err != nil {
		return check, false, err
	}
	if check.Repos, err = scanStrings(rows); err != nil {
		return check, false, err
	}
	return check, true, nil
}

// blobsAfter lists up to limit recorded blobs whose digest sorts after the
// given one.
func (r *Registry) blobsAfter(ctx context.Context, after string, limit int) ([]blob.Info, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT sha256, size FROM blobs WHERE sha256 > ? ORDER BY sha256 LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []blob.Info
	for rows.Next() {
		var b blob.Info
		if err := rows.Scan(&b.SHA256, &b.Size); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
