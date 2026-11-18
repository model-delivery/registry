// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// Metadata backups over the API (docs/api.md). All of it needs the admin
// role: a snapshot contains every token's hash and the full catalogue.

// apiBackupNow takes a backup and stores it next to the model files.
func (s *Server) apiBackupNow(w http.ResponseWriter, r *http.Request) {
	b, info, err := s.reg.Backup(r.Context(), s.cfg.BackupKeep)
	if err != nil {
		s.failBackup(w, r, err)
		return
	}
	s.log.Info("metadata backup stored", "name", b.Name, "bytes", b.Size, "repos", info.Repos, "revisions", info.Revisions, "via", "api")
	writeJSON(w, http.StatusCreated, map[string]any{"backup": b, "contents": info})
}

func (s *Server) apiListBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.reg.Backups(r.Context())
	if err != nil {
		s.failBackup(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

// apiSnapshot streams a fresh snapshot of the metadata database, for
// keeping a copy somewhere of the caller's choosing.
func (s *Server) apiSnapshot(w http.ResponseWriter, r *http.Request) {
	dir, err := s.reg.Scratch()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "snapshot.db")
	info, err := s.reg.Snapshot(r.Context(), path)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := os.Open(path) //nolint:gosec // a file this handler just created.
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	name := "registry-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Cache-Control", "no-store")
	s.log.Info("metadata snapshot downloaded", "bytes", info.Size, "repos", info.Repos, "revisions", info.Revisions)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) failBackup(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, registry.ErrNoBackupStore):
		writeError(w, http.StatusNotImplemented, err.Error()+"; download a snapshot instead (modelctl backup download)")
	case errors.Is(err, blob.ErrArchiveNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.fail(w, r, err)
	}
}
