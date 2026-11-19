// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// EventKind says what a line of mirror progress reports.
type EventKind string

// The lines of a mirror's progress, in the order they are sent: one start,
// a file for each file, then done or error.
const (
	EventStart EventKind = "start"
	EventFile  EventKind = "file"
	EventDone  EventKind = "done"
	EventError EventKind = "error"
)

// FileStatus says how a file's content came to be stored.
type FileStatus string

// How a mirrored file was obtained.
const (
	// StatusDownloaded: fetched from the upstream and verified.
	StatusDownloaded FileStatus = "downloaded"
	// StatusReused: its bytes were already stored.
	StatusReused FileStatus = "reused"
)

// Event is one line of mirror progress.
type Event struct {
	Event EventKind `json:"event"`
	Repo  string    `json:"repo,omitempty"`
	// RepoType is the type of Repo, on the start and done lines; absent for
	// a model.
	RepoType registry.RepoType `json:"repo_type,omitzero"`
	SHA      string            `json:"sha,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Path     string            `json:"path,omitempty"`
	// Status of a file event.
	Status          FileStatus `json:"status,omitempty"`
	Size            int64      `json:"size,omitempty"`
	Files           int        `json:"files,omitempty"`
	Bytes           int64      `json:"bytes,omitempty"`
	DownloadedBytes int64      `json:"downloaded_bytes,omitempty"`
	ReusedBytes     int64      `json:"reused_bytes,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// Result is what a finished mirror did.
type Result struct {
	// Repo is the key (registry.RepoKey) the revision was recorded under:
	// the upstream's name for a model, "datasets/" and its name for a
	// dataset.
	Repo string
	SHA  string
	// Ref is the branch that now points at the revision; empty when the
	// revision was asked for by id.
	Ref   string
	Files int
	// Bytes is the size of the revision: DownloadedBytes that were fetched
	// plus ReusedBytes that were already stored.
	Bytes           int64
	DownloadedBytes int64
	ReusedBytes     int64
}

// doneEvent is the last line of a successful mirror's progress.
func (r Result) doneEvent() Event {
	name, typ := eventRepo(r.Repo)
	return Event{Event: EventDone, Repo: name, RepoType: typ, SHA: r.SHA, Ref: r.Ref, Files: r.Files,
		Bytes: r.Bytes, DownloadedBytes: r.DownloadedBytes, ReusedBytes: r.ReusedBytes}
}

// eventRepo is how a progress line names the repository with key key: by
// its name, with the type set only for a dataset. A model's lines are what
// they were before datasets existed, down to the Go value.
func eventRepo(key string) (string, registry.RepoType) {
	typ, name := registry.SplitRepoKey(key)
	if typ.IsZero() {
		return name, ""
	}
	return name, typ
}

// DefaultConcurrency is how many files a mirror fetches at once unless
// told otherwise.
const DefaultConcurrency = 4

// defaultRevision is what a mirror copies when no revision is named.
const defaultRevision = "main"

// maxStoredInfo is the largest upstream model description kept with a
// revision. It is provenance, shown back by the model info route; a larger
// one is not stored at all, so a revision's metadata stays small whatever
// the upstream sends.
const maxStoredInfo = 1 << 20

// Plan is a resolved upstream revision, ready to be copied.
type Plan struct {
	up       *Upstream
	revision string
	// Repo is the key (registry.RepoKey) the revision is recorded under:
	// the upstream's canonical name, of the type that was asked for.
	Repo string
	// Type is the repository's type.
	Type    registry.RepoType
	Info    Info
	Entries []Entry
}

// Prepare resolves repo@revision upstream and lists its files. repo is a
// repository key (registry.RepoKey), so a dataset is asked for as
// "datasets/namespace/name". Nothing is written locally, so callers can
// report upstream errors before committing to a long transfer.
func Prepare(ctx context.Context, up *Upstream, repo, revision string) (*Plan, error) {
	if err := registry.ValidateRepoKey(repo); err != nil {
		return nil, err
	}
	typ, _ := registry.SplitRepoKey(repo)
	if revision == "" {
		revision = defaultRevision
	}
	// Everything that can fail from here on is the upstream's doing: it
	// could not be reached, or what it answered cannot be mirrored.
	info, err := up.info(ctx, repo, revision)
	if err != nil {
		return nil, blame(err)
	}
	// The hub answers with the canonical id, which can differ from what was
	// asked for after a rename. Store under the canonical one.
	if err := registry.ValidateRepoName(info.ID); err != nil {
		return nil, blame(fmt.Errorf("upstream repository id %q is not supported: %w", info.ID, err))
	}
	if !registry.IsSHA(info.SHA) {
		return nil, blame(fmt.Errorf("upstream revision id %q is not a 40-character hash", info.SHA))
	}
	key := registry.RepoKey(typ, info.ID)
	entries, err := up.tree(ctx, key, info.SHA)
	if err != nil {
		return nil, blame(err)
	}
	if len(entries) == 0 {
		return nil, blame(fmt.Errorf("upstream revision %s has no files", info.SHA))
	}
	for _, e := range entries {
		if err := registry.ValidatePath(e.Path); err != nil {
			return nil, blame(err)
		}
	}
	return &Plan{up: up, revision: revision, Repo: key, Type: typ, Info: info, Entries: entries}, nil
}

// storedInfo is the upstream's model description as it is kept with the
// revision: all of it, or none when it is over maxStoredInfo.
func (p *Plan) storedInfo() string {
	if len(p.Info.Raw) > maxStoredInfo {
		return ""
	}
	return string(p.Info.Raw)
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
//
// concurrency is how many files are fetched at once; below 1 means
// DefaultConcurrency. progress, when not nil, is called from several
// goroutines with a start event, one event per file, and a done event once
// the revision is recorded. A failure is the returned error, never an
// event: reporting it is the caller's business.
func (p *Plan) Execute(ctx context.Context, reg *registry.Registry, concurrency int, progress func(Event)) (Result, error) {
	if progress == nil {
		progress = func(Event) {}
	}
	if concurrency < 1 {
		concurrency = DefaultConcurrency
	}
	repo, sha := p.Repo, p.Info.SHA
	name, typ := eventRepo(repo)
	progress(Event{Event: EventStart, Repo: name, RepoType: typ, SHA: sha, Files: len(p.Entries), Bytes: p.Bytes()})

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
				f, fetched, err := ensureBlob(ctx, p.up, reg, repo, sha, e, nil)
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
				status := StatusReused
				if fetched {
					status = StatusDownloaded
					downloaded += f.Size
				} else {
					reused += f.Size
				}
				mu.Unlock()
				progress(Event{Event: EventFile, Path: e.Path, Size: f.Size, Status: status})
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
		return Result{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	in := registry.CommitInput{
		Repo:         repo,
		SHA:          sha,
		Source:       registry.SourceMirror,
		Upstream:     p.up.endpoint(),
		UpstreamInfo: p.storedInfo(),
		Message:      "Mirrored from " + p.up.endpoint(),
		Files:        files,
	}
	// A named revision becomes a local branch pointing at the same id, so
	// "main" keeps working offline. A raw revision id is recorded as is.
	if !registry.IsSHA(p.revision) {
		in.Branch = p.revision
	}
	// The revision may already be recorded, perhaps by pull-through with
	// some files still to fetch. Every file is stored now, so the registry
	// completes it and moves the branch, in one transaction. Only when it
	// holds no such revision is this a new one to record whole; any other
	// failure is the mirror's failure.
	_, err := reg.CompleteRevision(ctx, registry.CompleteInput{
		Repo: repo, SHA: sha, Branch: in.Branch, Upstream: in.Upstream, Files: files,
	})
	if errors.Is(err, registry.ErrRevisionNotFound) {
		_, err = reg.Commit(ctx, in)
	}
	if err != nil {
		return Result{}, err
	}
	res := Result{Repo: repo, SHA: sha, Ref: in.Branch, Files: len(files),
		Bytes: downloaded + reused, DownloadedBytes: downloaded, ReusedBytes: reused}
	progress(res.doneEvent())
	return res, nil
}

func (p *Plan) lazyInput(branch string) registry.LazyInput {
	files := make([]registry.File, len(p.Entries))
	for i, e := range p.Entries {
		files[i] = registry.File{Path: e.Path, Size: e.Size, GitOID: e.OID, LFS: e.LFSSHA256 != "", SHA256: e.LFSSHA256}
	}
	return registry.LazyInput{
		Repo: p.Repo, Branch: branch, SHA: p.Info.SHA, Files: files,
		Upstream: p.up.endpoint(), UpstreamInfo: p.storedInfo(), Message: "Mirrored from " + p.up.endpoint(),
	}
}

// RecordLazy records the revision from its upstream file listing without
// fetching any content. Every file is listed with its size and digest;
// FetchFile brings in one file's bytes when it is first wanted.
//
// This is what pull-through calls, on a reader's request, so it only ever
// records into a repository that is new or that came from an upstream; for
// one created in this registry it fails with registry.ErrLocalRepo.
func (p *Plan) RecordLazy(ctx context.Context, reg *registry.Registry) (registry.Commit, error) {
	branch := ""
	if !registry.IsSHA(p.revision) {
		branch = p.revision
	}
	in := p.lazyInput(branch)
	in.OnlyFromUpstream = true
	return reg.CommitLazy(ctx, in)
}

// FetchFile fetches the content of one pending file of revision c from up,
// verifies it against the digest recorded for it, and marks it stored.
func FetchFile(ctx context.Context, up *Upstream, reg *registry.Registry, c registry.Commit, f registry.File) error {
	_, err := FetchFileFollowed(ctx, up, reg, c, f, nil)
	return err
}

// FetchFileFollowed is FetchFile with a tail on the download, so the
// caller's readers can follow the bytes while they arrive (blob.Tail says
// what such a reader may rely on). The tail is used only if the content has
// to be downloaded; ending it is the caller's business either way.
//
// downloaded reports whether the content came from the upstream now, and
// not from a blob the registry already stored.
func FetchFileFollowed(ctx context.Context, up *Upstream, reg *registry.Registry, c registry.Commit, f registry.File, tail *blob.Tail) (downloaded bool, err error) {
	e := Entry{Path: f.Path, Size: f.Size, OID: f.GitOID}
	if f.LFS {
		e.LFSSHA256 = f.SHA256
	}
	got, downloaded, err := ensureBlob(ctx, up, reg, c.Key(), c.SHA, e, tail)
	if err != nil {
		return false, err
	}
	return downloaded, reg.Fulfil(ctx, c, f.Path, got.SHA256)
}

// ensureBlob makes sure one upstream file's bytes are stored, downloading
// and verifying them only if they are not already present. repo is the
// repository's key. tail, when not nil, follows the download.
func ensureBlob(ctx context.Context, up *Upstream, reg *registry.Registry, repo, sha string, e Entry, tail *blob.Tail) (registry.File, bool, error) {
	f := registry.File{Path: e.Path, GitOID: e.OID, LFS: e.LFSSHA256 != ""}
	want := blob.Expect{Size: e.Size, Tail: tail}
	if f.LFS {
		if !blob.ValidDigest(e.LFSSHA256) {
			return f, false, blame(fmt.Errorf("upstream lists a malformed sha256 %q", e.LFSSHA256))
		}
		// For LFS files the hub publishes the SHA-256 of the content.
		want.SHA256 = e.LFSSHA256
		// Reusing a stored blob marks it as just used, as a push does, so
		// garbage collection cannot remove it while this revision is still
		// being put together.
		if info, ok, err := reg.TouchBlob(ctx, e.LFSSHA256); err != nil {
			return f, false, err
		} else if ok {
			f.SHA256, f.Size = info.SHA256, info.Size
			return f, false, nil
		}
	} else {
		// For regular files the listed id is the git blob id of the content:
		// a SHA-1, and the only digest the upstream publishes for them. That
		// is enough to check a download against, but not to pick out stored
		// content by, since anyone with the write role chooses what is
		// stored. So a stored blob stands in only if a mirrored revision
		// already uses it, meaning it once came from an upstream under this
		// same id. Otherwise the file is downloaded; such files are small.
		want.GitOID = e.OID
		if info, ok, err := reg.FindMirroredBlob(ctx, e.OID, e.Size); err != nil {
			return f, false, err
		} else if ok {
			if _, _, err := reg.TouchBlob(ctx, info.SHA256); err != nil {
				return f, false, err
			}
			f.SHA256, f.Size = info.SHA256, info.Size
			return f, false, nil
		}
	}
	// Two things can fail below, and they are told apart: reading from the
	// upstream (marked by blamedBody) or content that is not what it listed
	// is the upstream's failure; staging and recording the bytes is ours.
	body := blamedBody{up.open(ctx, repo, sha, e.Path)}
	defer func() { _ = body.Close() }()
	info, err := reg.PutBlob(ctx, body, want)
	if errors.Is(err, blob.ErrMismatch) {
		err = blame(err)
	}
	if err != nil {
		return f, false, err
	}
	f.SHA256, f.Size = info.SHA256, info.Size
	return f, true, nil
}
