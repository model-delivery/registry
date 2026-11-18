// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package datadir is the one place that knows how a registry's data
// directory is laid out. Every other package asks it for a path instead of
// joining names itself.
//
//	data/
//	  registry.db   the metadata database (plus SQLite's -wal and -shm)
//	  blobs/        stored files, with the filesystem backend
//	  backups/      metadata backups, with the filesystem backend
//	  tmp/          uploads being received, snapshots being taken
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir is the path of a data directory.
type Dir string

// Database is the metadata database file. SQLite keeps its write-ahead log
// and that log's index beside it, under this name plus "-wal" and "-shm".
func (d Dir) Database() string { return filepath.Join(string(d), "registry.db") }

// Blobs is where the filesystem backend keeps stored files.
func (d Dir) Blobs() string { return filepath.Join(string(d), "blobs") }

// Backups is where the filesystem backend keeps metadata backups.
func (d Dir) Backups() string { return filepath.Join(string(d), "backups") }

// Staging is local scratch space: uploads are received and verified here
// before they are published, and snapshots are written here before they are
// stored or sent. Nothing in it outlives the operation that made it.
func (d Dir) Staging() string { return filepath.Join(string(d), "tmp") }

// ClearStaging removes whatever a previous run left in the staging
// directory: uploads that were cut off, snapshots that were never sent.
//
// Only the process that owns the data directory may call it, once, before
// it starts receiving anything: that is `modelctl serve` at start. A command
// that merely reads the directory (restore) must not, because a running
// server's uploads in progress live here too.
func (d Dir) ClearStaging() error {
	if err := os.RemoveAll(d.Staging()); err != nil {
		return fmt.Errorf("clear staging directory: %w", err)
	}
	if err := os.MkdirAll(d.Staging(), 0o750); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	return nil
}
