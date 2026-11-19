// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// undeletable is a store whose backups cannot be removed.
type undeletable struct{ blob.Store }

func (undeletable) DeleteArchive(context.Context, string) error {
	return errors.New("remove /srv/registry/backups/old.db: permission denied")
}

type backupAnswer struct {
	Backup struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"backup"`
	Contents struct {
		Repos int `json:"repos"`
	} `json:"contents"`
	Warning *string `json:"warning"`
	Error   string  `json:"error"`
}

// A backup that was stored is answered as a success. When old backups
// could not then be removed the answer says so in a "warning" field; it
// used to be "500 internal error" for a backup that exists.
func TestBackupNowWarnsWhenOldBackupsCannotBeRemoved(t *testing.T) {
	t.Parallel()
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logs := &regtest.LogBuffer{}
	in := regtest.StartWith(t, server.Config{AnonymousRead: true, BackupKeep: 1, Logger: slog.New(slog.NewTextHandler(logs, nil))}, undeletable{fs})

	take := func() (int, backupAnswer) {
		t.Helper()
		resp := do(t, http.MethodPost, in.URL+"/api/v1/backups", in.Admin, nil, nil)
		var out backupAnswer
		if err := json.Unmarshal([]byte(body(t, resp)), &out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}

	// The first backup has nothing to remove: no warning field at all.
	status, first := take()
	if status != http.StatusCreated || first.Backup.Name == "" || first.Warning != nil {
		t.Fatalf("first backup: status %d, %+v", status, first)
	}

	// The second is stored; the first cannot be removed.
	status, second := take()
	if status != http.StatusCreated {
		t.Fatalf("a backup that was stored was answered %d: %s", status, second.Error)
	}
	if second.Backup.Name == "" || second.Backup.Name == first.Backup.Name || second.Backup.Size == 0 {
		t.Fatalf("second backup = %+v, want it described, under its own name", second.Backup)
	}
	if second.Warning == nil || !strings.Contains(*second.Warning, "was stored") || !strings.Contains(*second.Warning, "could not be removed") {
		t.Fatalf("warning = %v, want it to say the backup was stored and old ones were not removed", second.Warning)
	}
	// The reason is for the operator's log, not the response.
	if strings.Contains(*second.Warning, "/srv/registry") {
		t.Fatalf("the warning carries a server path: %s", *second.Warning)
	}
	if got := logs.String(); !strings.Contains(got, "permission denied") || !strings.Contains(got, "level=WARN") {
		t.Fatalf("the reason was not logged:\n%s", got)
	}

	// Both are there to be listed.
	resp := do(t, http.MethodGet, in.URL+"/api/v1/backups", in.Admin, nil, nil)
	var listing struct {
		Backups []struct {
			Name string `json:"name"`
		} `json:"backups"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &listing); err != nil || len(listing.Backups) != 2 {
		t.Fatalf("listing = %+v, %v; want both backups", listing, err)
	}
}
