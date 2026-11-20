// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/model-delivery/registry/internal/registry"
)

// Mirroring refs: every branch and tag of a repository, or one branch, as
// the upstream lists them now. This is what `modelctl mirror --all-refs`
// runs, and what the server's scheduled sync runs for a followed
// repository.
//
// It only ever adds. A ref the upstream no longer has is left as it is, a
// branch moves only forward to what the upstream points it at, and a tag
// never moves: an upstream tag that names another revision than the
// registry's tag of that name is reported and left. Each ref is mirrored on
// its own, so one that fails costs only itself.

// RefOutcome says what mirroring did with one ref.
type RefOutcome string

// What became of a ref.
const (
	// RefUnchanged: the registry already had the ref where the upstream
	// has it, with every file stored.
	RefUnchanged RefOutcome = "unchanged"
	// RefMirrored: the revision is stored and the ref points at it: a new
	// branch or tag, a branch moved, or a revision whose files were not all
	// stored completed.
	RefMirrored RefOutcome = "mirrored"
	// RefTagMoved: the upstream's tag names another revision than the
	// registry's, which stays where it is.
	RefTagMoved RefOutcome = "tag_moved"
	// RefConflict: the name is a branch on one side and a tag on the other,
	// or both upstream; the registry's ref is left as it is.
	RefConflict RefOutcome = "conflict"
	// RefDeletedUpstream: the registry has the ref and the upstream no
	// longer does. It stays.
	RefDeletedUpstream RefOutcome = "deleted_upstream"
	// RefFailed: the ref could not be mirrored; nothing of it was recorded.
	RefFailed RefOutcome = "failed"
)

// RefResult is what mirroring did with one ref.
type RefResult struct {
	Name    string           `json:"name"`
	Kind    registry.RefKind `json:"kind"`
	Outcome RefOutcome       `json:"outcome"`
	// SHA is the revision the upstream's ref points at; absent for a ref
	// deleted upstream.
	SHA string `json:"sha,omitempty"`
	// Previous is the revision the registry's ref pointed at before, when
	// it had the ref.
	Previous string `json:"previous,omitempty"`
	// Detail explains every outcome but unchanged and mirrored. For a
	// failure it is what Options.Describe made of Err.
	Detail string `json:"detail,omitempty"`
	// Err is why a failed ref failed.
	Err error `json:"-"`
}

// event is the progress line that reports r.
func (r RefResult) event() Event {
	return Event{Event: EventRef, Ref: r.Name, Kind: r.Kind, Outcome: r.Outcome, SHA: r.SHA, Previous: r.Previous, Detail: r.Detail}
}

// RefsResult is what a mirror of refs did.
type RefsResult struct {
	// Repo is the repository's key.
	Repo string
	// Refs is one result per ref: those the upstream lists, then those
	// only the registry has.
	Refs            []RefResult
	DownloadedBytes int64
	ReusedBytes     int64
}

// Changed reports whether any ref was mirrored.
func (r RefsResult) Changed() bool {
	for _, ref := range r.Refs {
		if ref.Outcome == RefMirrored {
			return true
		}
	}
	return false
}

// Failed is the refs that failed.
func (r RefsResult) Failed() []RefResult {
	var out []RefResult
	for _, ref := range r.Refs {
		if ref.Outcome == RefFailed {
			out = append(out, ref)
		}
	}
	return out
}

// RefsDone is the last progress line of a mirror of refs that ran to the
// end, whatever became of each ref: a done line, with every ref's result.
type RefsDone struct {
	Event
	Refs []RefResult `json:"refs"`
}

// DoneEvent is the last progress line of the mirror that did r.
func (r RefsResult) DoneEvent() RefsDone {
	name, typ := eventRepo(r.Repo)
	return RefsDone{Event: Event{Event: EventDone, Repo: name, RepoType: typ,
		Bytes: r.DownloadedBytes + r.ReusedBytes, DownloadedBytes: r.DownloadedBytes, ReusedBytes: r.ReusedBytes}, Refs: r.Refs}
}

// ListRefs asks the upstream for the branches and tags of the repository
// whose key is key, branches first. Every failure is the upstream's.
func ListRefs(ctx context.Context, up *Upstream, key string, slot Slot) ([]UpstreamRef, error) {
	if err := registry.ValidateRepoKey(key); err != nil {
		return nil, err
	}
	var listed []UpstreamRef
	err := hold(ctx, slot, func() (err error) {
		listed, err = up.refs(ctx, key)
		return err
	})
	return listed, blame(err)
}

// MirrorRefs mirrors the refs spec names of the repository whose key is
// key, as listed (ListRefs, asked just before): one branch, or every branch
// and tag. Each ref's revision is fetched in full and verified (files
// already stored are not fetched again), and the ref recorded: a branch as
// a branch, a tag as a tag.
//
// The error is for a mirror that could not start, because the registry's
// refs could not be read. Everything after that is reported per ref in the
// result. Progress, when set in opt, receives each revision's start and
// file lines and a ref line per ref; the done line is the caller's to send
// (RefsResult.DoneEvent).
func MirrorRefs(ctx context.Context, up *Upstream, reg *registry.Registry, key string, spec registry.FollowSpec, listed []UpstreamRef, opt Options) (RefsResult, error) {
	if err := registry.ValidateRepoKey(key); err != nil {
		return RefsResult{}, err
	}
	if spec.AllRefs == (spec.Branch != "") {
		return RefsResult{}, fmt.Errorf("%w: mirror one branch or every branch and tag", registry.ErrInvalid)
	}
	held := map[string]registry.Ref{}
	switch repo, err := reg.GetRepo(ctx, key); {
	case errors.Is(err, registry.ErrRepoNotFound):
	case err != nil:
		return RefsResult{}, err
	default:
		refs, err := reg.Refs(ctx, repo)
		if err != nil {
			return RefsResult{}, err
		}
		for _, r := range refs {
			held[r.Name] = r
		}
	}

	m := refMirror{up: up, reg: reg, key: key, opt: opt, progress: opt.Progress}
	if m.progress == nil {
		m.progress = func(Event) {}
	}
	if m.opt.Describe == nil {
		m.opt.Describe = func(err error) string { return err.Error() }
	}
	out := RefsResult{Repo: key}
	report := func(r RefResult) {
		out.Refs = append(out.Refs, r)
		m.progress(r.event())
	}

	upstream := map[string]registry.RefKind{}
	for _, u := range listed {
		if kind, twice := upstream[u.Name]; twice {
			// Branches come first, so this is a tag with a branch's name.
			if spec.AllRefs {
				report(RefResult{Name: u.Name, Kind: u.Kind, Outcome: RefConflict, SHA: u.SHA, Previous: held[u.Name].SHA,
					Detail: fmt.Sprintf("the upstream has a %s and a %s of this name; only the %s is mirrored", kind, u.Kind, kind)})
			}
			continue
		}
		upstream[u.Name] = u.Kind
		if !spec.AllRefs && (u.Name != spec.Branch || u.Kind != registry.KindBranch) {
			continue
		}
		report(m.one(ctx, u, held[u.Name]))
	}
	if !spec.AllRefs && upstream[spec.Branch] != registry.KindBranch {
		r := RefResult{Name: spec.Branch, Kind: registry.KindBranch, Outcome: RefDeletedUpstream, Previous: held[spec.Branch].SHA,
			Detail: "the upstream has no branch of this name; the registry's is left as it is"}
		if _, isTag := upstream[spec.Branch]; isTag {
			r.Outcome, r.Detail = RefConflict, "the upstream has a tag of this name, not a branch; the registry's ref is left as it is"
		}
		report(r)
	}
	if spec.AllRefs {
		var gone []string
		for name := range held {
			if _, listed := upstream[name]; !listed {
				gone = append(gone, name)
			}
		}
		sort.Strings(gone)
		for _, name := range gone {
			h := held[name]
			report(RefResult{Name: name, Kind: h.Kind, Outcome: RefDeletedUpstream, Previous: h.SHA,
				Detail: "the upstream no longer has this " + string(h.Kind) + "; the registry keeps it"})
		}
	}
	out.DownloadedBytes, out.ReusedBytes = m.downloaded, m.reused
	return out, nil
}

// refMirror is one run of MirrorRefs.
type refMirror struct {
	up       *Upstream
	reg      *registry.Registry
	key      string
	opt      Options
	progress func(Event)

	downloaded, reused int64
}

// one decides what to do with the upstream ref u, given the registry's ref
// of the same name (the zero Ref when it has none), and does it.
func (m *refMirror) one(ctx context.Context, u UpstreamRef, h registry.Ref) RefResult {
	res := RefResult{Name: u.Name, Kind: u.Kind, SHA: u.SHA, Previous: h.SHA}
	if err := registry.ValidateRefName(u.Name); err != nil {
		return m.failed(res, blame(fmt.Errorf("the upstream's %s %q cannot be recorded here: %w", u.Kind, u.Name, err)))
	}
	switch {
	case h.Name != "" && h.Kind != u.Kind:
		res.Outcome = RefConflict
		res.Detail = fmt.Sprintf("it is a %s here and a %s upstream; the registry's %s is left as it is", h.Kind, u.Kind, h.Kind)
		return res
	case h.Kind == registry.KindTag && h.SHA != u.SHA:
		res.Outcome = RefTagMoved
		res.Detail = (&TagMovedError{Repo: m.key, Tag: u.Name, Local: h.SHA, Upstream: u.SHA}).Error()
		return res
	}
	c, err := m.reg.Resolve(ctx, m.key, u.SHA)
	complete := false
	switch {
	case err == nil:
		n, err := m.reg.PendingCount(ctx, c)
		if err != nil {
			return m.failed(res, err)
		}
		complete = n == 0
	case !errors.Is(err, registry.ErrRevisionNotFound) && !errors.Is(err, registry.ErrRepoNotFound):
		return m.failed(res, err)
	}
	if complete && h.SHA == u.SHA {
		res.Outcome = RefUnchanged
		return res
	}
	to := target{name: u.Name, kind: u.Kind}
	if complete {
		// The revision is stored in full already (another ref, or an earlier
		// state of this one, brought it): only the ref is recorded.
		if _, err := m.reg.CompleteRevision(ctx, registry.CompleteInput{Repo: m.key, SHA: u.SHA, Branch: to.branch(), Tags: to.tags()}); err != nil {
			return m.failed(res, err)
		}
		res.Outcome = RefMirrored
		return res
	}
	var plan *Plan
	if err := hold(ctx, m.opt.ListingSlot, func() (err error) {
		plan, err = Prepare(ctx, m.up, m.key, u.SHA)
		return err
	}); err != nil {
		return m.failed(res, err)
	}
	if plan.Repo != m.key {
		return m.failed(res, blame(fmt.Errorf("the upstream now names %s %s; mirror it under that name", m.key, plan.Repo)))
	}
	plan.target = to
	done, err := plan.ExecuteWith(ctx, m.reg, Options{
		Concurrency: m.opt.Concurrency, FileSlot: m.opt.FileSlot,
		// The done line of each revision is not the end of this mirror.
		Progress: func(e Event) {
			if e.Event != EventDone {
				m.progress(e)
			}
		},
	})
	if err != nil {
		return m.failed(res, err)
	}
	m.downloaded += done.DownloadedBytes
	m.reused += done.ReusedBytes
	res.Outcome = RefMirrored
	return res
}

// failed is res, failed with err.
func (m *refMirror) failed(res RefResult, err error) RefResult {
	res.Outcome, res.Err, res.Detail = RefFailed, err, m.opt.Describe(err)
	return res
}
