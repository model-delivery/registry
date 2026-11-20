// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
)

// Keeping mirrors up to date: `mirror --all-refs`, `mirror --follow`,
// `follow list|stop` and `sync`.

// mirrorAllRefs mirrors every branch and tag of the repository with key
// key, prints how each ref went, and with follow then follows them all. It
// fails when any ref failed, after reporting every one.
func (c *command) mirrorAllRefs(key, upstreamToken string, follow bool) error {
	e := c.e
	done, err := c.client().MirrorAllRefs(e.ctx, key, upstreamToken, func(ev client.MirrorEvent) {
		if c.jsonOut {
			return
		}
		switch ev.Event {
		case client.EventStart:
			fmt.Fprintf(e.stderr, "Mirroring %s at revision %s: %d files, %s\n", shown(ev.Repo, ev.RepoType), ev.SHA, ev.Files, kit.HumanBytes(ev.Bytes))
		case client.EventFile:
			c.progress(ev.Status, ev.Size, ev.Path)
		}
	})
	if err != nil {
		return err
	}
	if c.jsonOut {
		if err := c.printJSON(done); err != nil {
			return err
		}
	} else {
		c.printRefs(done.Refs)
		fmt.Fprintf(e.stdout, "Mirrored %s: %s; %s downloaded, %s already stored\n", shown(done.Repo, done.RepoType),
			refCounts(done.Refs), kit.HumanBytes(done.DownloadedBytes), kit.HumanBytes(done.ReusedBytes))
	}
	if err := refsFailed(done.Refs); err != nil {
		return err
	}
	if follow {
		return c.startFollowing(key, client.FollowOptions{AllRefs: true})
	}
	return nil
}

// printRefs prints a table of what happened to each ref, and a warning
// for each one that was left as it was.
func (c *command) printRefs(refs []client.RefResult) {
	tw := c.table()
	fmt.Fprintln(tw, "REF\tKIND\tOUTCOME\tREVISION")
	for _, r := range refs {
		sha := r.SHA
		if sha == "" {
			sha = r.Previous
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Kind, r.Outcome, sha)
	}
	_ = tw.Flush()
	for _, r := range refs {
		switch r.Outcome {
		case client.RefTagMoved, client.RefConflict, client.RefDeletedUpstream:
			c.warn(fmt.Sprintf("%s %s: %s", r.Kind, r.Name, r.Detail))
		case client.RefFailed:
			fmt.Fprintf(c.e.stderr, "Failed: %s %s: %s\n", r.Kind, r.Name, r.Detail)
		}
	}
}

// refCounts says how many refs had each outcome, in a fixed order.
func refCounts(refs []client.RefResult) string {
	counts := map[string]int{}
	for _, r := range refs {
		counts[r.Outcome]++
	}
	var parts []string
	for _, o := range []string{client.RefMirrored, client.RefUnchanged, client.RefTagMoved, client.RefConflict, client.RefDeletedUpstream, client.RefFailed} {
		if counts[o] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[o], strings.ReplaceAll(o, "_", " ")))
		}
	}
	if len(parts) == 0 {
		return "no refs"
	}
	return strings.Join(parts, ", ")
}

// refsFailed is the error of a run in which some refs failed, naming them.
func refsFailed(refs []client.RefResult) error {
	var failed []string
	for _, r := range refs {
		if r.Outcome == client.RefFailed {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d refs could not be mirrored (%s); the others were, and running the command again retries the rest",
		len(failed), len(refs), strings.Join(failed, ", "))
}

// startFollowing follows the repository with key key and says so.
func (c *command) startFollowing(key string, opt client.FollowOptions) error {
	f, err := c.client().Follow(c.e.ctx, key, opt)
	if err != nil {
		return fmt.Errorf("mirrored, but could not follow it: %w", err)
	}
	if !c.jsonOut {
		fmt.Fprintf(c.e.stdout, "Following %s: %s\n", shown(f.Name, f.Type), follows(f.FollowEntry))
	}
	return nil
}

// follows says what a follow list entry follows.
func follows(f client.FollowEntry) string {
	if f.AllRefs {
		return "every branch and tag"
	}
	return "branch " + f.Branch
}

// when shows a time in a table, or "never" for the zero time.
func when(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return localTime(t)
}

func cmdFollow(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("expected a subcommand: list or stop (start following with modelctl mirror <repo> --follow)")
	}
	switch args[0] {
	case "list":
		c := newClientCommand(e, "follow list", "follow list")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		list, err := c.client().ListFollows(e.ctx)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(list.Raw)
		}
		if len(list.Follows) == 0 {
			fmt.Fprintln(e.stdout, "Nothing is followed. Try: modelctl mirror <namespace/name> --follow")
			return nil
		}
		tw := c.table()
		fmt.Fprintln(tw, "REPOSITORY\tFOLLOWS\tLAST CHECK\tLAST SUCCESS\tLAST CHANGE\tLAST ERROR")
		for _, f := range list.Follows {
			lastErr := f.LastError
			if lastErr == "" {
				lastErr = "-"
			} else if f.Failures > 1 {
				lastErr = fmt.Sprintf("%s (%d checks in a row)", lastErr, f.Failures)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", shown(f.Name, f.Type), follows(f), when(f.LastCheck), when(f.LastSuccess), when(f.LastChange), lastErr)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if list.IntervalSeconds > 0 {
			fmt.Fprintf(e.stdout, "\nThe registry checks each one every %s.\n", time.Duration(list.IntervalSeconds)*time.Second)
		} else {
			fmt.Fprintln(e.stdout, "\nThe registry has no sync schedule (REGISTRY_SYNC_INTERVAL); check them with modelctl sync.")
		}
		return nil
	case "stop":
		c := newClientCommand(e, "follow stop", "follow stop <namespace/name> [flags]")
		typeFlag := c.repoTypeFlag()
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		key, err := repoKey(pos[0], *typeFlag)
		if err != nil {
			return err
		}
		if err := c.client().Unfollow(e.ctx, key); err != nil {
			return err
		}
		if c.jsonOut {
			// The registry answers with no document, so this one is the
			// command's own.
			return c.printJSON(map[string]any{"repo": key, "followed": false})
		}
		typ, _ := repoType(*typeFlag) // checked by repoKey
		fmt.Fprintf(e.stdout, "No longer following %s; everything it holds stays\n", shown(pos[0], typ))
		return nil
	}
	return usagef("unknown subcommand %q: expected list or stop", args[0])
}

func cmdSync(e *env, args []string) error {
	c := newClientCommand(e, "sync", "sync <namespace/name> | --all [flags]")
	all := c.Bool("all", false, "start a check of every followed repository and return at once")
	typeFlag := c.repoTypeFlag()
	pos, err := c.parse(args, 0, 1)
	if err != nil {
		return err
	}
	if *all {
		if len(pos) != 0 {
			return usagef("--all checks every followed repository; name none")
		}
		q, err := c.client().SyncAll(e.ctx)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(q.Raw)
		}
		fmt.Fprintf(e.stdout, "Started a check of %d followed repositories; modelctl follow list shows how they went\n", len(q.Queued))
		return nil
	}
	if len(pos) != 1 {
		return usagef("name the repository to check, or pass --all")
	}
	key, err := repoKey(pos[0], *typeFlag)
	if err != nil {
		return err
	}
	res, err := c.client().Sync(e.ctx, key)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(res.Raw)
	}
	c.printRefs(res.Refs)
	state := "already up to date"
	if res.Changed {
		state = "updated: " + kit.HumanBytes(res.DownloadedBytes) + " downloaded"
	}
	fmt.Fprintf(e.stdout, "Checked %s against the upstream: %s\n", shown(res.Name, res.Type), state)
	return nil
}
