// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the modelctl command line: the server
// (modelctl serve) and the client commands that talk to it.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/registry"
)

// Version is set at build time with -ldflags.
var Version = "dev"

const usage = `modelctl - self-hosted model registry, compatible with Hugging Face clients

Server:
  modelctl serve                      run the registry

Client:
  modelctl mirror <repo>              copy a repository from the upstream hub into the registry
  modelctl push <dir> <repo>          upload a directory as a new revision
  modelctl pull <repo>                download a revision and verify it
  modelctl list                       list repositories
  modelctl inspect <repo>             show revisions, refs and files
  modelctl delete <repo>              delete a repository and free its storage
  modelctl token create|list|revoke   manage access tokens
  modelctl gc                         remove stored files nothing references
  modelctl backup now|list|download   back up the registry's metadata

Recovery (registry stopped):
  modelctl restore <file>|--latest    put a metadata backup back in place
  modelctl version

Client commands read MODELCTL_ENDPOINT (default http://localhost:8080) and
MODELCTL_TOKEN. Add --json to any of them for machine-readable output.
Run "modelctl <command> --help" for a command's flags.
`

// env is what a command needs from its surroundings.
type env struct {
	ctx    context.Context
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
}

// Run executes modelctl with args (without the program name) and returns
// the process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	e := &env{ctx: ctx, stdout: stdout, stderr: stderr, getenv: getenv}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	commands := map[string]func(*env, []string) error{
		"serve": cmdServe, "mirror": cmdMirror, "push": cmdPush, "pull": cmdPull, "list": cmdList,
		"inspect": cmdInspect, "delete": cmdDelete, "token": cmdToken, "gc": cmdGC,
		"backup": cmdBackup, "restore": cmdRestore,
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, Version)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "modelctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	err := cmd(e, args[1:])
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, new(usageError)):
		fmt.Fprintf(stderr, "modelctl %s: %v\n", args[0], err)
		return 2
	default:
		fmt.Fprintf(stderr, "modelctl %s: %v\n", args[0], err)
		return 1
	}
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// command is a flag set plus the options every client command shares.
type command struct {
	*flag.FlagSet
	e        *env
	endpoint string
	token    string
	jsonOut  bool
}

func (e *env) envOr(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}
	return fallback
}

func newCommand(e *env, name, synopsis string) *command {
	c := &command{FlagSet: flag.NewFlagSet(name, flag.ContinueOnError), e: e}
	c.SetOutput(e.stderr)
	c.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: modelctl %s\n\nflags:\n", synopsis)
		c.PrintDefaults()
	}
	return c
}

func newClientCommand(e *env, name, synopsis string) *command {
	c := newCommand(e, name, synopsis)
	c.StringVar(&c.endpoint, "endpoint", e.envOr("MODELCTL_ENDPOINT", "http://localhost:8080"), "registry URL (env MODELCTL_ENDPOINT)")
	c.StringVar(&c.token, "token", "", "access token (env MODELCTL_TOKEN)")
	c.BoolVar(&c.jsonOut, "json", false, "print machine-readable JSON")
	return c
}

// parse accepts flags before, between and after positional arguments, and
// checks how many positionals were given.
func (c *command) parse(args []string, minArgs, maxArgs int) ([]string, error) {
	var pos []string
	for {
		if err := c.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		if c.NArg() == 0 {
			break
		}
		pos = append(pos, c.Arg(0))
		args = c.Args()[1:]
	}
	if len(pos) < minArgs || len(pos) > maxArgs {
		c.Usage()
		return nil, usagef("expected %s, got %d", argCount(minArgs, maxArgs), len(pos))
	}
	return pos, nil
}

func argCount(lo, hi int) string {
	if lo == hi {
		return fmt.Sprintf("%d argument(s)", lo)
	}
	return fmt.Sprintf("%d to %d arguments", lo, hi)
}

func (c *command) client() *client.Client {
	token := c.token
	if token == "" {
		token = c.e.getenv("MODELCTL_TOKEN")
	}
	return client.New(c.endpoint, token)
}

func (c *command) printJSON(v any) error {
	enc := json.NewEncoder(c.e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func short(sha string) string { return sha[:min(len(sha), 12)] }

// checkRepo rejects a malformed repository name before any request is made.
func checkRepo(name string) error {
	if err := registry.ValidateRepoName(name); err != nil {
		return usagef("%q is not a repository name: use namespace/name, for example my-team/my-model", name)
	}
	return nil
}

// ---- mirror ----

func cmdMirror(e *env, args []string) error {
	c := newClientCommand(e, "mirror", "mirror <namespace/name> [flags]")
	revision := c.String("revision", "main", "upstream branch, tag or revision id")
	upToken := c.String("upstream-token", "", "token for the upstream hub, for private or gated repositories (env MODELCTL_UPSTREAM_TOKEN, then HF_TOKEN)")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if err := checkRepo(pos[0]); err != nil {
		return err
	}
	if *upToken == "" {
		*upToken = e.envOr("MODELCTL_UPSTREAM_TOKEN", e.getenv("HF_TOKEN"))
	}
	done, err := c.client().Mirror(e.ctx, pos[0], *revision, *upToken, func(ev client.MirrorEvent) {
		if c.jsonOut {
			return
		}
		switch ev.Event {
		case "start":
			fmt.Fprintf(e.stderr, "Mirroring %s at revision %s: %d files, %s\n", ev.Repo, ev.SHA, ev.Files, humanBytes(ev.Bytes))
		case "file":
			fmt.Fprintf(e.stderr, "  %-10s %10s  %s\n", ev.Status, humanBytes(ev.Size), ev.Path)
		}
	})
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(done)
	}
	fmt.Fprintf(e.stdout, "Mirrored %s at revision %s\n", done.Repo, done.SHA)
	fmt.Fprintf(e.stdout, "%d files verified: %s downloaded, %s already stored\n",
		done.Files, humanBytes(done.DownloadedBytes), humanBytes(done.ReusedBytes))
	return nil
}

// ---- push ----

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdPush(e *env, args []string) error {
	c := newClientCommand(e, "push", "push <directory> <namespace/name> [flags]")
	branch := c.String("branch", "main", "branch to move to the new revision")
	message := c.String("message", "", "revision message")
	merge := c.Bool("merge", false, "keep the branch's existing files and add or replace these (default: the directory becomes the whole revision)")
	var tags stringList
	c.Var(&tags, "tag", "tag to create at the new revision; repeatable; tags never move")
	pos, err := c.parse(args, 2, 2)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(pos[0]); err != nil || !fi.IsDir() {
		return usagef("%s is not a directory", pos[0])
	}
	if err := checkRepo(pos[1]); err != nil {
		return err
	}
	res, err := c.client().Push(e.ctx, pos[0], pos[1], client.PushOptions{
		Branch: *branch, Tags: tags, Message: *message, Merge: *merge,
		Progress: func(p string, size int64, uploaded bool) {
			if c.jsonOut {
				return
			}
			status := "exists"
			if uploaded {
				status = "uploaded"
			}
			fmt.Fprintf(e.stderr, "  %-10s %10s  %s\n", status, humanBytes(size), p)
		},
	})
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(res)
	}
	fmt.Fprintf(e.stdout, "Pushed %s at revision %s (%s)\n", res.Repo, res.SHA, *branch)
	return nil
}

// ---- pull ----

func cmdPull(e *env, args []string) error {
	c := newClientCommand(e, "pull", "pull <namespace/name> [flags]")
	revision := c.String("revision", "main", "branch, tag or revision id")
	dir := c.String("dir", "", "target directory (default: ./<name>)")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if err := checkRepo(pos[0]); err != nil {
		return err
	}
	target := *dir
	if target == "" {
		_, target, _ = strings.Cut(pos[0], "/")
	}
	rev, err := c.client().Pull(e.ctx, pos[0], *revision, target, func(p string, size int64, fetched bool) {
		if c.jsonOut {
			return
		}
		status := "up to date"
		if fetched {
			status = "downloaded"
		}
		fmt.Fprintf(e.stderr, "  %-10s %10s  %s\n", status, humanBytes(size), p)
	})
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(map[string]any{"repo": rev.Repo, "sha": rev.SHA, "dir": target, "files": len(rev.Files)})
	}
	fmt.Fprintf(e.stdout, "Pulled %s at revision %s into %s (%d files verified)\n", rev.Repo, rev.SHA, target, len(rev.Files))
	return nil
}

// ---- list / inspect / delete ----

func cmdList(e *env, args []string) error {
	c := newClientCommand(e, "list", "list [flags]")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	var out struct {
		Repos []struct {
			Name        string    `json:"name"`
			Upstream    string    `json:"upstream"`
			Revisions   int       `json:"revisions"`
			StoredBytes int64     `json:"stored_bytes"`
			UpdatedAt   time.Time `json:"updated_at"`
		} `json:"repos"`
	}
	raw, err := c.client().Raw(e.ctx, http.MethodGet, "/api/v1/repos", nil)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	if len(out.Repos) == 0 {
		fmt.Fprintln(e.stdout, "No repositories yet. Try: modelctl mirror <namespace/name>")
		return nil
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tSOURCE\tREVISIONS\tSIZE\tUPDATED")
	for _, r := range out.Repos {
		source := "push"
		if r.Upstream != "" {
			source = "mirror"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.Name, source, r.Revisions, humanBytes(r.StoredBytes), r.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func cmdInspect(e *env, args []string) error {
	c := newClientCommand(e, "inspect", "inspect <namespace/name> [flags]")
	revision := c.String("revision", "", "also list the files of this branch, tag or revision id")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if err := checkRepo(pos[0]); err != nil {
		return err
	}
	cl := c.client()
	raw, err := cl.Raw(e.ctx, http.MethodGet, "/api/v1/repos/"+pos[0], nil)
	if err != nil {
		return err
	}
	var rev *client.Revision
	if *revision != "" {
		r, err := cl.Revision(e.ctx, pos[0], *revision)
		if err != nil {
			return err
		}
		rev = &r
	}
	if c.jsonOut {
		if rev != nil {
			return c.printJSON(map[string]any{"repository": raw, "revision": rev})
		}
		return c.printJSON(raw)
	}
	var d struct {
		Name      string `json:"name"`
		Upstream  string `json:"upstream"`
		Refs      []struct{ Name, Kind, SHA string }
		Revisions []struct {
			SHA       string    `json:"sha"`
			Source    string    `json:"source"`
			Files     int       `json:"files"`
			Bytes     int64     `json:"bytes"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Repository: %s\n", d.Name)
	if d.Upstream != "" {
		fmt.Fprintf(e.stdout, "Mirror of:  %s/%s\n", d.Upstream, d.Name)
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nREF\tKIND\tREVISION")
	for _, r := range d.Refs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Kind, r.SHA)
	}
	fmt.Fprintln(tw, "\nREVISION\tSOURCE\tFILES\tSIZE\tSTORED")
	for _, r := range d.Revisions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.SHA, r.Source, r.Files, humanBytes(r.Bytes), r.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	if rev != nil {
		fmt.Fprintf(tw, "\nFILES AT %s\tSIZE\tSHA256\n", short(rev.SHA))
		for _, f := range rev.Files {
			digest := f.SHA256
			if f.Pending {
				digest = "(not fetched yet: fetched on first download)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Path, humanBytes(f.Size), digest)
		}
	}
	return tw.Flush()
}

func cmdDelete(e *env, args []string) error {
	c := newClientCommand(e, "delete", "delete <namespace/name> --yes [flags]")
	yes := c.Bool("yes", false, "confirm: this removes every revision and cannot be undone")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if err := checkRepo(pos[0]); err != nil {
		return err
	}
	if !*yes {
		return usagef("deleting %s removes every revision and cannot be undone; pass --yes to confirm", pos[0])
	}
	var res struct {
		Blobs int   `json:"blobs"`
		Bytes int64 `json:"bytes"`
	}
	raw, err := c.client().Raw(e.ctx, http.MethodDelete, "/api/v1/repos/"+pos[0], nil)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(raw)
	}
	_ = json.Unmarshal(raw, &res)
	fmt.Fprintf(e.stdout, "Deleted %s: freed %s in %d stored files\n", pos[0], humanBytes(res.Bytes), res.Blobs)
	return nil
}

func cmdGC(e *env, args []string) error {
	c := newClientCommand(e, "gc", "gc [flags]")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	raw, err := c.client().Raw(e.ctx, http.MethodPost, "/api/v1/gc", nil)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(raw)
	}
	var res struct {
		Blobs int   `json:"blobs"`
		Bytes int64 `json:"bytes"`
	}
	_ = json.Unmarshal(raw, &res)
	fmt.Fprintf(e.stdout, "Removed %d unreferenced stored files, freed %s\n", res.Blobs, humanBytes(res.Bytes))
	return nil
}

// ---- token ----

func cmdToken(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("expected a subcommand: create, list or revoke")
	}
	switch args[0] {
	case "create":
		c := newClientCommand(e, "token create", "token create <name> [--role read|write|admin]")
		role := c.String("role", "read", "read (download), write (also push and mirror) or admin (also delete and manage tokens)")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		raw, err := c.client().Raw(e.ctx, http.MethodPost, "/api/v1/tokens", map[string]string{"name": pos[0], "role": *role})
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(raw)
		}
		var out struct{ Token string }
		_ = json.Unmarshal(raw, &out)
		fmt.Fprintf(e.stderr, "Created %s token %q. It is shown only this once:\n", *role, pos[0])
		fmt.Fprintln(e.stdout, out.Token)
		return nil
	case "list":
		c := newClientCommand(e, "token list", "token list")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		raw, err := c.client().Raw(e.ctx, http.MethodGet, "/api/v1/tokens", nil)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(raw)
		}
		var out struct {
			Tokens []struct {
				Name      string    `json:"name"`
				Role      string    `json:"role"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"tokens"`
		}
		_ = json.Unmarshal(raw, &out)
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tCREATED")
		for _, t := range out.Tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	case "revoke":
		c := newClientCommand(e, "token revoke", "token revoke <name>")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		if _, err := c.client().Raw(e.ctx, http.MethodDelete, "/api/v1/tokens/"+pos[0], nil); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if !c.jsonOut {
			fmt.Fprintf(e.stdout, "Revoked token %q\n", pos[0])
		}
		return nil
	}
	return usagef("unknown subcommand %q: expected create, list or revoke", args[0])
}
