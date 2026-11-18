// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"fmt"
	"sync"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// Event is one line of mirror progress.
type Event struct {
	Event string `json:"event"` // start | file | done | error
	Repo  string `json:"repo,omitempty"`
	SHA   string `json:"sha,omitempty"`
	Ref   string `json:"ref,omitempty"`
	Path  string `json:"path,omitempty"`
	// Status of a file: "downloaded" or "reused" (bytes already stored).
	Status          string `json:"status,omitempty"`
	Size            int64  `json:"size,omitempty"`
	Files           int    `json:"files,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`
	DownloadedBytes int64  `json:"downloaded_bytes,omitempty"`
	ReusedBytes     int64  `json:"reused_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Plan is a resolved upstream revision, ready to be copied.
type Plan struct {
	up       *Upstream
	revision string
	Info     Info
	Entries  []Entry
}

// Prepare resolves repo@revision upstream and lists its files. Nothing is
// written locally, so callers can report upstream errors before committing
// to a long transfer.
func Prepare(ctx context.Context, up *Upstream, repo, revision string) (*Plan, error) {
	if err := registry.ValidateRepoName(repo); err != nil {
		return nil, err
	}
	if revision == "" {
		revision = "main"
	}
	info, err := up.Info(ctx, repo, revision)
	if err != nil {
		return nil, err
	}
	// The hub answers with the canonical id, which can differ from what was
	// asked for after a rename. Store under the canonical one.
	if err := registry.ValidateRepoName(info.ID); err != nil {
		return nil, fmt.Errorf("upstream repository id %q is not supported: %w", info.ID, err)
	}
	if !registry.IsSHA(info.SHA) {
		return nil, fmt.Errorf("upstream revision id %q is not a 40-character hash", info.SHA)
	}
	entries, err := up.Tree(ctx, info.ID, info.SHA)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("upstream revision %s has no files", info.SHA)
	}
	for _, e := range entries {
		if err := registry.ValidatePath(e.Path); err != nil {
			return nil, err
		}
	}
	return &Plan{up: up, revision: revision, Info: info, Entries: entries}, nil
}

// Bytes is the total size of the revision's files.
func (p *Plan) Bytes() int64 {
	var n int64
	for _, e := range p.Entries {
		n += e.Size
	}
	return n
}

// Execute copies the plan into reg and records the revision under its
// upstream id. Files whose bytes are already stored are not fetched again.
// The revision only becomes visible once every file is stored and verified.
func (p *Plan) Execute(ctx context.Context, reg *registry.Registry, concurrency int, progress func(Event)) (Event, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	if concurrency < 1 {
		concurrency = 4
	}
	repo, sha := p.Info.ID, p.Info.SHA
	progress(Event{Event: "start", Repo: repo, SHA: sha, Files: len(p.Entries), Bytes: p.Bytes()})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	files := make([]registry.File, len(p.Entries))
	var (
		mu         sync.Mutex
		firstErr   error
		downloaded int64
		reused     int64
		wg         sync.WaitGroup
	)
	jobs := make(chan int)
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				e := p.Entries[i]
				f, fetched, err := ensureBlob(ctx, p.up, reg, repo, sha, e)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", e.Path, err)
						cancel()
					}
					mu.Unlock()
					continue
				}
				files[i] = f
				status := "reused"
				if fetched {
					status = "downloaded"
					downloaded += f.Size
				} else {
					reused += f.Size
				}
				mu.Unlock()
				progress(Event{Event: "file", Path: e.Path, Size: f.Size, Status: status})
			}
		}()
	}
feed:
	for i := range p.Entries {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return Event{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}

	in := registry.CommitInput{
		Repo:         repo,
		SHA:          sha,
		Source:       registry.SourceMirror,
		Upstream:     p.up.endpoint(),
		UpstreamInfo: string(p.Info.Raw),
		Message:      "Mirrored from " + p.up.endpoint(),
		Files:        files,
	}
	// A named revision becomes a local branch pointing at the same id, so
	// "main" keeps working offline. A raw revision id is recorded as is.
	if !registry.IsSHA(p.revision) {
		in.Branch = p.revision
	}
	if existing, err := reg.Resolve(ctx, repo, sha); err == nil {
		// The revision is already recorded, perhaps by pull-through with
		// some files still to fetch. Every file is stored now, so complete
		// it, and move the branch.
		for _, f := range files {
			if err := reg.Fulfil(ctx, existing, f.Path, f.SHA256); err != nil {
				return Event{}, fmt.Errorf("%s: %w", f.Path, err)
			}
		}
		if _, err := reg.CommitLazy(ctx, p.lazyInput(in.Branch)); err != nil {
			return Event{}, err
		}
	} else if _, err := reg.Commit(ctx, in); err != nil {
		return Event{}, err
	}
	return Event{Event: "done", Repo: repo, SHA: sha, Ref: in.Branch, Files: len(files),
		Bytes: downloaded + reused, DownloadedBytes: downloaded, ReusedBytes: reused}, nil
}

func (p *Plan) lazyInput(branch string) registry.LazyInput {
	files := make([]registry.File, len(p.Entries))
	for i, e := range p.Entries {
		files[i] = registry.File{Path: e.Path, Size: e.Size, GitOID: e.OID, LFS: e.LFSSHA256 != "", SHA256: e.LFSSHA256}
	}
	return registry.LazyInput{
		Repo: p.Info.ID, Branch: branch, SHA: p.Info.SHA, Files: files,
		Upstream: p.up.endpoint(), UpstreamInfo: string(p.Info.Raw), Message: "Mirrored from " + p.up.endpoint(),
	}
}

// RecordLazy records the revision from its upstream file listing without
// fetching any content. Every file is listed with its size and digest;
// FetchFile brings in one file's bytes when it is first wanted.
func (p *Plan) RecordLazy(ctx context.Context, reg *registry.Registry) (registry.Commit, error) {
	branch := ""
	if !registry.IsSHA(p.revision) {
		branch = p.revision
	}
	return reg.CommitLazy(ctx, p.lazyInput(branch))
}

// FetchFile fetches the content of one pending file of revision c from up,
// verifies it against the digest recorded for it, and marks it stored.
func FetchFile(ctx context.Context, up *Upstream, reg *registry.Registry, c registry.Commit, f registry.File) error {
	e := Entry{Path: f.Path, Size: f.Size, OID: f.GitOID}
	if f.LFS {
		e.LFSSHA256 = f.SHA256
	}
	got, _, err := ensureBlob(ctx, up, reg, c.Repo, c.SHA, e)
	if err != nil {
		return err
	}
	return reg.Fulfil(ctx, c, f.Path, got.SHA256)
}

// ensureBlob makes sure one upstream file's bytes are stored, downloading
// and verifying them only if they are not already present.
func ensureBlob(ctx context.Context, up *Upstream, reg *registry.Registry, repo, sha string, e Entry) (registry.File, bool, error) {
	f := registry.File{Path: e.Path, GitOID: e.OID, LFS: e.LFSSHA256 != ""}
	want := blob.Expect{Size: e.Size}
	if f.LFS {
		if !blob.ValidDigest(e.LFSSHA256) {
			return f, false, fmt.Errorf("upstream lists a malformed sha256 %q", e.LFSSHA256)
		}
		// For LFS files the hub publishes the SHA-256 of the content.
		want.SHA256 = e.LFSSHA256
		if info, ok, err := reg.StatBlob(ctx, e.LFSSHA256); err != nil {
			return f, false, err
		} else if ok {
			f.SHA256, f.Size = info.SHA256, info.Size
			return f, false, nil
		}
	} else {
		// For regular files the listed id is the git blob id of the content.
		want.GitOID = e.OID
		if info, ok, err := reg.FindBlobByGitOID(ctx, e.OID, e.Size); err != nil {
			return f, false, err
		} else if ok {
			f.SHA256, f.Size = info.SHA256, info.Size
			return f, false, nil
		}
	}
	body := up.Open(ctx, repo, sha, e.Path)
	defer func() { _ = body.Close() }()
	info, err := reg.PutBlob(ctx, body, want)
	if err != nil {
		return f, false, err
	}
	f.SHA256, f.Size = info.SHA256, info.Size
	return f, true, nil
}
