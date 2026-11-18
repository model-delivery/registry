// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/model-delivery/registry/internal/registry"
)

// modelctl backup talks to a running registry. modelctl restore works on a
// data directory directly, with the registry stopped.

func cmdBackup(e *env, args []string) error {
	if len(args) == 0 {
		return usagef("expected a subcommand: now, list or download")
	}
	switch args[0] {
	case "now":
		c := newClientCommand(e, "backup now", "backup now")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		raw, err := c.client().Raw(e.ctx, http.MethodPost, "/api/v1/backups", nil)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(raw)
		}
		var out struct {
			Backup struct {
				Name string `json:"name"`
				Size int64  `json:"size"`
			} `json:"backup"`
			Contents struct {
				Repos     int `json:"repos"`
				Revisions int `json:"revisions"`
			} `json:"contents"`
		}
		_ = json.Unmarshal(raw, &out)
		fmt.Fprintf(e.stdout, "Stored backup %s (%s): %d repositories, %d revisions\n",
			out.Backup.Name, humanBytes(out.Backup.Size), out.Contents.Repos, out.Contents.Revisions)
		return nil

	case "list":
		c := newClientCommand(e, "backup list", "backup list")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		raw, err := c.client().Raw(e.ctx, http.MethodGet, "/api/v1/backups", nil)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(raw)
		}
		var out struct {
			Backups []struct {
				Name      string    `json:"name"`
				Size      int64     `json:"size"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"backups"`
		}
		_ = json.Unmarshal(raw, &out)
		if len(out.Backups) == 0 {
			fmt.Fprintln(e.stdout, "No stored backups. Take one with: modelctl backup now")
			return nil
		}
		tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "BACKUP\tSIZE\tCREATED")
		for _, b := range out.Backups {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, humanBytes(b.Size), b.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()

	case "download":
		c := newClientCommand(e, "backup download", "backup download <file>")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		n, err := c.client().Download(e.ctx, "/api/v1/backup/snapshot", pos[0])
		if err != nil {
			return err
		}
		// Check what arrived before calling it a backup.
		info, err := registry.VerifySnapshot(e.ctx, pos[0])
		if err != nil {
			_ = os.Remove(pos[0])
			return fmt.Errorf("the downloaded snapshot failed its check and was removed: %w", err)
		}
		if c.jsonOut {
			return c.printJSON(map[string]any{"file": pos[0], "size": n, "contents": info})
		}
		fmt.Fprintf(e.stdout, "Saved a verified snapshot to %s (%s): %d repositories, %d revisions\n",
			pos[0], humanBytes(n), info.Repos, info.Revisions)
		return nil
	}
	return usagef("unknown subcommand %q: expected now, list or download", args[0])
}

func cmdRestore(e *env, args []string) error {
	c := newCommand(e, "restore", "restore <snapshot file> | --latest | --name <backup>   [flags]")
	data := c.String("data", e.envOr("REGISTRY_DATA", "./data"), "data directory to restore into (env REGISTRY_DATA)")
	storage := c.String("storage", e.envOr("REGISTRY_STORAGE", "filesystem"), "where stored backups are kept: filesystem or s3 (env REGISTRY_STORAGE), configured as for serve")
	latest := c.Bool("latest", false, "restore the newest backup in the registry's storage")
	name := c.String("name", "", "restore this stored backup (see modelctl backup list)")
	replace := c.Bool("replace", false, "restore over an existing database, which is kept beside it")
	pos, err := c.parse(args, 0, 1)
	if err != nil {
		return err
	}
	fromStorage := *latest || *name != ""
	if fromStorage == (len(pos) == 1) {
		c.Usage()
		return usagef("give a snapshot file, or --latest, or --name: exactly one")
	}

	snapshot := ""
	source := ""
	if fromStorage {
		// The storage backend, quietly: its start-up line is not this command's output.
		store, _, err := openStore(e, *data, *storage, slog.New(slog.DiscardHandler))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(*data, "tmp"), 0o750); err != nil {
			return err
		}
		snapshot = filepath.Join(*data, "tmp", "restore-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".db")
		defer func() { _ = os.Remove(snapshot) }()
		source, err = registry.FetchBackup(e.ctx, store, *name, snapshot)
		if err != nil {
			return err
		}
	} else {
		snapshot, source = pos[0], pos[0]
	}

	info, previous, err := registry.Restore(e.ctx, *data, snapshot, *replace)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "Restored %s into %s: %d repositories, %d revisions\n", source, *data, info.Repos, info.Revisions)
	if previous != "" {
		fmt.Fprintf(e.stdout, "The database that was there is kept at %s\n", previous)
	}
	fmt.Fprintln(e.stdout, "Start the registry with: modelctl serve")
	return nil
}
