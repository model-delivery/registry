// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// Metadata backups over the API (docs/api.md). All of it needs the admin
// role: a snapshot contains every token's hash and the full catalogue.

// pruneWarning is what a caller is told when a backup was stored and old
// ones could not then be removed. The reason is in the server's log.
const pruneWarning = "the backup was stored, but old backups beyond the number to keep could not be removed; the server log has the reason"

// apiBackupNow takes a backup and stores it next to the model files.
func (s *Server) apiBackupNow(w http.ResponseWriter, r *http.Request) {
	res, err := s.reg.Backup(r.Context(), s.cfg.BackupKeep)
	if err != nil {
		s.failBackup(w, r, err)
		return
	}
	s.log.Info("metadata backup stored", "name", res.Backup.Name, "bytes", res.Backup.Size,
		"repos", res.Contents.Repos, "revisions", res.Contents.Revisions, "via", "api")
	out := map[string]any{"backup": res.Backup, "contents": res.Contents}
	if res.PruneErr != nil {
		// The backup exists, so this is a success with something to add.
		s.log.Warn("old metadata backups could not be removed", "keep", s.cfg.BackupKeep, "error", res.PruneErr)
		out["warning"] = pruneWarning
	}
	writeJSON(w, http.StatusCreated, out)
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
	snapshot, info, err := s.reg.OpenSnapshot(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Closing it removes the temporary file it reads from.
	defer func() { _ = snapshot.Close() }()
	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="`+registry.BackupName(time.Now())+`"`)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	h.Set("Cache-Control", "no-store")
	s.log.Info("metadata snapshot downloaded", "bytes", info.Size, "repos", info.Repos, "revisions", info.Revisions)
	http.ServeContent(w, r, "", time.Time{}, snapshot)
}

func (s *Server) failBackup(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, blob.ErrArchiveNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.fail(w, r, err)
}
