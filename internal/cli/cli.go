// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the modelctl command line: the server
// (modelctl serve) and the client commands that talk to it.
//
// A client command reads its flags, makes one call on internal/client and
// prints the answer. It builds no request and decodes no response itself.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/kit"
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
  modelctl verify                     re-read every stored file and check it against its digest
  modelctl backup now|list|download   back up the registry's metadata

Recovery (registry stopped):
  modelctl restore <file>|--latest    put a metadata backup back in place
  modelctl version

Client commands read MODELCTL_ENDPOINT (default http://localhost:8080) and
MODELCTL_TOKEN. Add --json to any of them for machine-readable output.
Run "modelctl <command> --help" for a command's flags.
`

// Defaults and formats of the client commands.
const (
	// defaultEndpoint is the registry a client command talks to when neither
	// --endpoint nor MODELCTL_ENDPOINT names one.
	defaultEndpoint = "http://localhost:8080"
	// defaultBranch is the branch a push moves and a pull or mirror reads
	// when none is named.
	defaultBranch = "main"
	// timeLayout is how a time is shown in a table, in the local zone.
	timeLayout = "2006-01-02 15:04"
	// The columns of a table: at least tableMinWidth wide, tabs every
	// tableTabWidth, tablePadding spaces between them.
	tableMinWidth = 0
	tableTabWidth = 4
	tablePadding  = 2
)

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
		"backup": cmdBackup, "restore": cmdRestore, "verify": cmdVerify,
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
	// notes is printed after the flags by --help.
	notes string
	// envErr is the first environment variable found with a value that
	// cannot be read. It is reported by parse, after the flags, so that
	// --help still works.
	envErr error
}

func (e *env) envOr(key, fallback string) string {
	if v := e.getenv(key); v != "" {
		return v
	}
	return fallback
}

// option resolves a client option the way every one of them is resolved:
// what the flag was given, else the first of the environment variables
// keys that is set, else fallback. The environment is read after the flags
// are parsed and not as the flag's default, because a default is printed by
// --help, and two of these options are tokens.
func (e *env) option(flagValue, fallback string, keys ...string) string {
	if flagValue != "" {
		return flagValue
	}
	for _, key := range keys {
		if v := e.getenv(key); v != "" {
			return v
		}
	}
	return fallback
}

func newCommand(e *env, name, synopsis string) *command {
	c := &command{FlagSet: flag.NewFlagSet(name, flag.ContinueOnError), e: e}
	c.SetOutput(e.stderr)
	c.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: modelctl %s\n\nflags:\n", synopsis)
		c.PrintDefaults()
		if c.notes != "" {
			fmt.Fprintf(e.stderr, "\n%s", c.notes)
		}
	}
	return c
}

func newClientCommand(e *env, name, synopsis string) *command {
	c := newCommand(e, name, synopsis)
	c.StringVar(&c.endpoint, "endpoint", "", "registry URL (env MODELCTL_ENDPOINT; default "+defaultEndpoint+")")
	c.StringVar(&c.token, "token", "", "access token; prefer the environment variable MODELCTL_TOKEN, which does not show in the process list")
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
	if c.envErr != nil {
		return nil, c.envErr
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
	return client.New(
		c.e.option(c.endpoint, defaultEndpoint, "MODELCTL_ENDPOINT"),
		c.e.option(c.token, "", "MODELCTL_TOKEN"))
}

func (c *command) printJSON(v any) error {
	enc := json.NewEncoder(c.e.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table starts a table on standard output. Cells are separated by tabs and
// rows end with a newline; Flush writes it.
func (c *command) table() *tabwriter.Writer {
	return tabwriter.NewWriter(c.e.stdout, tableMinWidth, tableTabWidth, tablePadding, ' ', 0)
}

// warn prints what the registry said went wrong after it did what was
// asked. The command still succeeded.
func (c *command) warn(warning string) {
	if warning != "" {
		fmt.Fprintf(c.e.stderr, "Warning: %s\n", warning)
	}
}

// localTime shows t in the local zone, to the minute.
func localTime(t time.Time) string { return t.Local().Format(timeLayout) }

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
	revision := c.String("revision", defaultBranch, "upstream branch, tag or revision id")
	upToken := c.String("upstream-token", "", "token for the upstream hub, for private or gated repositories (env MODELCTL_UPSTREAM_TOKEN, then HF_TOKEN)")
	pos, err := c.parse(args, 1, 1)
	if err != nil {
		return err
	}
	if err := checkRepo(pos[0]); err != nil {
		return err
	}
	upstreamToken := e.option(*upToken, "", "MODELCTL_UPSTREAM_TOKEN", "HF_TOKEN")
	done, err := c.client().Mirror(e.ctx, pos[0], *revision, upstreamToken, func(ev client.MirrorEvent) {
		if c.jsonOut {
			return
		}
		switch ev.Event {
		case client.EventStart:
			fmt.Fprintf(e.stderr, "Mirroring %s at revision %s: %d files, %s\n", ev.Repo, ev.SHA, ev.Files, kit.HumanBytes(ev.Bytes))
		case client.EventFile:
			c.progress(ev.Status, ev.Size, ev.Path)
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
		done.Files, kit.HumanBytes(done.DownloadedBytes), kit.HumanBytes(done.ReusedBytes))
	return nil
}

// progress prints one file's line while a mirror, push or pull runs: what
// happened to it, its size and its path. Nothing is printed with --json.
func (c *command) progress(status string, size int64, path string) {
	if !c.jsonOut {
		fmt.Fprintf(c.e.stderr, "  %-10s %10s  %s\n", status, kit.HumanBytes(size), path)
	}
}

// ---- push ----

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdPush(e *env, args []string) error {
	c := newClientCommand(e, "push", "push <directory> <namespace/name> [flags]")
	branch := c.String("branch", defaultBranch, "branch to move to the new revision")
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
			status := "exists"
			if uploaded {
				status = "uploaded"
			}
			c.progress(status, size, p)
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
	revision := c.String("revision", defaultBranch, "branch, tag or revision id")
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
		status := "up to date"
		if fetched {
			status = "downloaded"
		}
		c.progress(status, size, p)
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
	list, err := c.client().ListRepos(e.ctx)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(list.Raw)
	}
	if len(list.Repos) == 0 {
		fmt.Fprintln(e.stdout, "No repositories yet. Try: modelctl mirror <namespace/name>")
		return nil
	}
	tw := c.table()
	fmt.Fprintln(tw, "REPOSITORY\tSOURCE\tREVISIONS\tSIZE\tUPDATED")
	for _, r := range list.Repos {
		source := "push"
		if r.Upstream != "" {
			source = "mirror"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.Name, source, r.Revisions, kit.HumanBytes(r.StoredBytes), localTime(r.UpdatedAt))
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
	d, err := cl.Repo(e.ctx, pos[0])
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
			return c.printJSON(map[string]any{"repository": d.Raw, "revision": rev})
		}
		return c.printJSON(d.Raw)
	}
	fmt.Fprintf(e.stdout, "Repository: %s\n", d.Name)
	if d.Upstream != "" {
		fmt.Fprintf(e.stdout, "Mirror of:  %s/%s\n", d.Upstream, d.Name)
	}
	tw := c.table()
	fmt.Fprintln(tw, "\nREF\tKIND\tREVISION")
	for _, r := range d.Refs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Kind, r.SHA)
	}
	fmt.Fprintln(tw, "\nREVISION\tSOURCE\tFILES\tSIZE\tSTORED")
	for _, r := range d.Revisions {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", r.SHA, r.Source, r.Files, kit.HumanBytes(r.Bytes), localTime(r.CreatedAt))
	}
	if rev != nil {
		fmt.Fprintf(tw, "\nFILES AT %s\tSIZE\tSHA256\n", kit.Short(rev.SHA))
		for _, f := range rev.Files {
			digest := f.SHA256
			if f.Pending {
				digest = "(not fetched yet: fetched on first download)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", f.Path, kit.HumanBytes(f.Size), digest)
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
	res, err := c.client().DeleteRepo(e.ctx, pos[0])
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(res.Raw)
	}
	fmt.Fprintf(e.stdout, "Deleted %s: freed %s in %d stored files\n", pos[0], kit.HumanBytes(res.Bytes), res.Blobs)
	c.warn(res.Warning)
	return nil
}

func cmdGC(e *env, args []string) error {
	c := newClientCommand(e, "gc", "gc [flags]")
	if _, err := c.parse(args, 0, 0); err != nil {
		return err
	}
	res, err := c.client().GC(e.ctx)
	if err != nil {
		return err
	}
	if c.jsonOut {
		return c.printJSON(res.Raw)
	}
	fmt.Fprintf(e.stdout, "Removed %d unreferenced stored files, freed %s\n", res.Blobs, kit.HumanBytes(res.Bytes))
	c.warn(res.Warning)
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
		role := c.String("role", string(registry.RoleRead), "read (download), write (also push and mirror) or admin (also delete and manage tokens)")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		created, err := c.client().CreateToken(e.ctx, pos[0], *role)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(created.Raw)
		}
		fmt.Fprintf(e.stderr, "Created %s token %q. It is shown only this once:\n", *role, pos[0])
		fmt.Fprintln(e.stdout, created.Token)
		return nil
	case "list":
		c := newClientCommand(e, "token list", "token list")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		list, err := c.client().ListTokens(e.ctx)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(list.Raw)
		}
		tw := c.table()
		fmt.Fprintln(tw, "NAME\tROLE\tCREATED")
		for _, t := range list.Tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, t.Role, localTime(t.CreatedAt))
		}
		return tw.Flush()
	case "revoke":
		c := newClientCommand(e, "token revoke", "token revoke <name>")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		if err := c.client().RevokeToken(e.ctx, pos[0]); err != nil {
			return err
		}
		if c.jsonOut {
			// The registry answers a revocation with no document, so this
			// one is the command's own.
			return c.printJSON(map[string]any{"name": pos[0], "revoked": true})
		}
		fmt.Fprintf(e.stdout, "Revoked token %q\n", pos[0])
		return nil
	}
	return usagef("unknown subcommand %q: expected create, list or revoke", args[0])
}
