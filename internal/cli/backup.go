// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"

	"github.com/model-delivery/registry/internal/kit"
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
		res, err := c.client().BackupNow(e.ctx)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(res.Raw)
		}
		fmt.Fprintf(e.stdout, "Stored backup %s (%s): %d repositories, %d revisions\n",
			res.Backup.Name, kit.HumanBytes(res.Backup.Size), res.Contents.Repos, res.Contents.Revisions)
		c.warn(res.Warning)
		return nil

	case "list":
		c := newClientCommand(e, "backup list", "backup list")
		if _, err := c.parse(args[1:], 0, 0); err != nil {
			return err
		}
		list, err := c.client().ListBackups(e.ctx)
		if err != nil {
			return err
		}
		if c.jsonOut {
			return c.printJSON(list.Raw)
		}
		if len(list.Backups) == 0 {
			fmt.Fprintln(e.stdout, "No stored backups. Take one with: modelctl backup now")
			return nil
		}
		tw := c.table()
		fmt.Fprintln(tw, "BACKUP\tSIZE\tCREATED")
		for _, b := range list.Backups {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, kit.HumanBytes(b.Size), localTime(b.CreatedAt))
		}
		return tw.Flush()

	case "download":
		c := newClientCommand(e, "backup download", "backup download <file>")
		pos, err := c.parse(args[1:], 1, 1)
		if err != nil {
			return err
		}
		n, err := c.client().DownloadSnapshot(e.ctx, pos[0])
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
			pos[0], kit.HumanBytes(n), info.Repos, info.Revisions)
		return nil
	}
	return usagef("unknown subcommand %q: expected now, list or download", args[0])
}

func cmdRestore(e *env, args []string) error {
	c := newCommand(e, "restore", "restore <snapshot file> | --latest | --name <backup>   [flags]")
	c.notes = restoreEnvOnlyNote
	data := c.String("data", e.envOr("REGISTRY_DATA", defaultDataDir), "data directory to restore into (env REGISTRY_DATA)")
	storage := c.storageFlags("where stored backups are kept: filesystem or s3 (env REGISTRY_STORAGE), configured as for serve")
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

	var res registry.Restored
	if fromStorage {
		// Opening the storage backend removes nothing, so this is safe
		// beside a registry that is running on the same data directory.
		store, err := storage.open(e, *data)
		if err != nil {
			return err
		}
		if res, err = registry.RestoreBackup(e.ctx, *data, store, *name, *replace); err != nil {
			return err
		}
	} else {
		info, previous, err := registry.Restore(e.ctx, *data, pos[0], *replace)
		if err != nil {
			return err
		}
		res = registry.Restored{Backup: pos[0], Contents: info, Previous: previous}
	}
	fmt.Fprintf(e.stdout, "Restored %s into %s: %d repositories, %d revisions\n", res.Backup, *data, res.Contents.Repos, res.Contents.Revisions)
	if res.Previous != "" {
		fmt.Fprintf(e.stdout, "The database that was there is kept at %s\n", res.Previous)
	}
	fmt.Fprintln(e.stdout, "Start the registry with: modelctl serve")
	return nil
}
