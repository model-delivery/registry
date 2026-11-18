// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
)

// An upload being received lives in the data directory's tmp/. `modelctl
// restore --latest` opens the storage only to read a backup, and used to
// empty tmp/ as a side effect of opening it. Only `serve` clears it, once,
// when it starts.
func TestOnlyServeClearsStaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := t.TempDir()

	// A registry with one stored backup.
	reg, err := registry.Open(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateRepo(ctx, "acme/model"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Backup(ctx, 0); err != nil {
		t.Fatal(err)
	}
	_ = reg.Close()

	inProgress := filepath.Join(data, "tmp", "stage-in-progress")
	if err := os.WriteFile(inProgress, []byte("half an upload"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"REGISTRY_DATA": data}
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"restore", "--latest", "--replace"}, &stdout, &stderr, func(k string) string { return env[k] })
	if code != 0 || !strings.Contains(stdout.String(), "1 repositories") {
		t.Fatalf("restore --latest: code %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(inProgress); err != nil {
		t.Fatalf("restore removed an upload in progress: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(data, "tmp"))
	if len(entries) != 1 {
		t.Fatalf("restore left %d entries in tmp/, want only the upload in progress", len(entries))
	}

	logs, addr, stop := runServe(t, data, map[string]string{"REGISTRY_ADMIN_TOKEN": backupAdmin})
	if addr == "" {
		t.Fatalf("serve did not start:\n%s", logs.String())
	}
	defer stop()
	if _, err := os.Stat(inProgress); !os.IsNotExist(err) {
		t.Fatalf("serve started without clearing what the last run left in tmp/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "tmp")); err != nil {
		t.Fatalf("tmp/ itself is gone: %v", err)
	}
}

// `backup now` reports a backup that was stored, and passes on the
// server's warning when something after storing it went wrong.
func TestBackupNowPrintsTheServersWarning(t *testing.T) {
	t.Parallel()
	const answer = `{"backup":{"name":"registry-20261118T120000Z.db","size":4096,"created_at":"2026-11-18T12:00:00Z"},` +
		`"contents":{"schema_version":2,"repos":3,"revisions":7,"size":4096}%s}`
	tests := []struct {
		name       string
		extra      string
		wantStderr string
	}{
		{"no warning", "", ""},
		{"with a warning", `,"warning":"the backup was stored, but old backups could not be removed"`,
			"Warning: the backup was stored, but old backups could not be removed\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/backups" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, strings.Replace(answer, "%s", tc.extra, 1))
			}))
			t.Cleanup(srv.Close)
			code, out, errOut := ctl(t, strings.TrimPrefix(srv.URL, "http://"), "md_some_admin_token", "backup", "now")
			if code != 0 {
				t.Fatalf("code %d: a stored backup is a success\n%s\n%s", code, out, errOut)
			}
			if !strings.Contains(out, "Stored backup registry-20261118T120000Z.db") || !strings.Contains(out, "3 repositories, 7 revisions") {
				t.Errorf("stdout = %q", out)
			}
			if errOut != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", errOut, tc.wantStderr)
			}
		})
	}
}
