// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
)

const backupAdmin = "md_admin_token_for_backup_tests"

// ctl runs a client command against addr and returns exit code and output.
func ctl(t *testing.T, addr, token string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := map[string]string{"MODELCTL_ENDPOINT": "http://" + addr, "MODELCTL_TOKEN": token}
	code := Run(context.Background(), args, &stdout, &stderr, func(k string) string { return env[k] })
	return code, stdout.String(), stderr.String()
}

// The scenario the feature exists for: a registry whose files are in a
// bucket loses its server and its disk. A new server, pointed at the same
// bucket, comes back with everything that was backed up.
func TestBackupThenLoseTheDiskThenRestore(t *testing.T) {
	t.Parallel()
	fake := blobtest.NewFakeS3(t)
	firstDisk := t.TempDir()

	logs, addr, stop := regtest.RunServe(t, Run, firstDisk, regtest.S3Env(fake, backupAdmin, nil))
	if addr == "" {
		t.Fatalf("serve did not start:\n%s", logs.String())
	}
	if code, _, errOut := ctl(t, addr, backupAdmin, "push", modelDir(t), "acme/tiny", "--tag", "v1"); code != 0 {
		t.Fatalf("push: %s", errOut)
	}
	if code, out, _ := ctl(t, addr, backupAdmin, "token", "create", "worker", "--role", "read"); code != 0 || !strings.HasPrefix(out, "md_") {
		t.Fatalf("token create: %d %q", code, out)
	}

	code, out, errOut := ctl(t, addr, backupAdmin, "backup", "list")
	if code != 0 || !strings.Contains(out, "No stored backups") {
		t.Fatalf("backup list before any backup: %d %q %q", code, out, errOut)
	}
	code, out, errOut = ctl(t, addr, backupAdmin, "backup", "now")
	if code != 0 || !strings.Contains(out, "1 repositories, 1 revisions") {
		t.Fatalf("backup now: %d %q %q", code, out, errOut)
	}
	code, out, _ = ctl(t, addr, backupAdmin, "backup", "list")
	if code != 0 || !strings.Contains(out, "registry-") {
		t.Fatalf("backup list: %d %q", code, out)
	}
	// The backup is in the bucket, beside the model files, not on the disk.
	var inBucket string
	for _, k := range fake.Keys(t) {
		if strings.HasPrefix(k, "prod/backups/registry-") {
			inBucket = k
		}
	}
	if inBucket == "" {
		t.Fatalf("no backup object in the bucket: %v", fake.Keys(t))
	}

	// A snapshot can also be taken away as a file, and is verified on arrival.
	saved := filepath.Join(t.TempDir(), "snapshot.db")
	code, out, errOut = ctl(t, addr, backupAdmin, "backup", "download", saved)
	if code != 0 || !strings.Contains(out, "verified snapshot") {
		t.Fatalf("backup download: %d %q %q", code, out, errOut)
	}
	if info, err := registry.VerifySnapshot(context.Background(), saved); err != nil || info.Repos != 1 {
		t.Fatalf("downloaded snapshot: %+v, %v", info, err)
	}

	// Backups hold every token hash and the whole catalogue: admins only.
	for _, args := range [][]string{{"backup", "now"}, {"backup", "list"}, {"backup", "download", filepath.Join(t.TempDir(), "x.db")}} {
		if code, _, _ := ctl(t, addr, "", args...); code == 0 {
			t.Errorf("%v succeeded without a token", args)
		}
	}

	// Work after the backup, which the backup cannot contain.
	if code, _, errOut := ctl(t, addr, backupAdmin, "push", modelDir(t), "acme/after-backup"); code != 0 {
		t.Fatalf("push: %s", errOut)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exit %d", code)
	}

	// The server and its disk are gone. A new, empty disk:
	newDisk := t.TempDir()

	t.Run("restore --latest brings the registry back from the bucket", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		env := regtest.S3Env(fake, backupAdmin, nil)
		code := Run(context.Background(), []string{"restore", "--latest", "--data", newDisk}, &stdout, &stderr, func(k string) string { return env[k] })
		if code != 0 || !strings.Contains(stdout.String(), "1 repositories, 1 revisions") {
			t.Fatalf("restore: %d %q %q", code, stdout.String(), stderr.String())
		}
		// Restoring again would overwrite a database; it is refused.
		stdout.Reset()
		stderr.Reset()
		if code := Run(context.Background(), []string{"restore", "--latest", "--data", newDisk}, &stdout, &stderr, func(k string) string { return env[k] }); code == 0 || !strings.Contains(stderr.String(), "--replace") {
			t.Fatalf("second restore: %d %q", code, stderr.String())
		}

		logs, addr, stop := regtest.RunServe(t, Run, newDisk, regtest.S3Env(fake, backupAdmin, nil))
		if addr == "" {
			t.Fatalf("serve after restore did not start:\n%s", logs.String())
		}
		defer stop()
		out := filepath.Join(t.TempDir(), "pulled")
		code, pulled, errOut := ctl(t, addr, "", "pull", "acme/tiny", "--revision", "v1", "--dir", out)
		if code != 0 || !strings.Contains(pulled, "2 files verified") {
			t.Fatalf("pull after restore: %d %q %q", code, pulled, errOut)
		}
		if got, err := os.ReadFile(filepath.Join(out, "sub", "weights.bin")); err != nil || len(got) != 4096 {
			t.Fatalf("restored model content: %v", err)
		}
		// What was pushed after the backup is not in it.
		if code, _, errOut := ctl(t, addr, "", "inspect", "acme/after-backup"); code == 0 || !strings.Contains(errOut, "repository not found") {
			t.Fatalf("a repository from after the backup is present: %d %q", code, errOut)
		}
	})

	t.Run("REGISTRY_RESTORE_IF_EMPTY does the same at start, once", func(t *testing.T) {
		emptyDisk := t.TempDir()
		logs, addr, stop := regtest.RunServe(t, Run, emptyDisk, regtest.S3Env(fake, backupAdmin, map[string]string{"REGISTRY_RESTORE_IF_EMPTY": "true"}))
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "restored the metadata database") {
			t.Fatalf("start did not restore:\n%s", logs.String())
		}
		if code, out, _ := ctl(t, addr, "", "list"); code != 0 || !strings.Contains(out, "acme/tiny") {
			t.Fatalf("list after automatic restore: %d %q", code, out)
		}
		// Push something, restart: the existing database is left alone.
		if code, _, errOut := ctl(t, addr, backupAdmin, "push", modelDir(t), "acme/new-work"); code != 0 {
			t.Fatalf("push: %s", errOut)
		}
		stop()
		logs, addr, stop = regtest.RunServe(t, Run, emptyDisk, regtest.S3Env(fake, backupAdmin, map[string]string{"REGISTRY_RESTORE_IF_EMPTY": "true"}))
		defer stop()
		if strings.Contains(logs.String(), "restored the metadata database") {
			t.Fatal("a restart restored over an existing database")
		}
		if code, out, _ := ctl(t, addr, "", "list"); code != 0 || !strings.Contains(out, "acme/new-work") {
			t.Fatalf("work done after the restore was lost on restart: %q", out)
		}
	})

	t.Run("with no backup to restore, a registry simply starts new", func(t *testing.T) {
		fresh := blobtest.NewFakeS3(t)
		logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), regtest.S3Env(fresh, backupAdmin, map[string]string{"REGISTRY_RESTORE_IF_EMPTY": "true"}))
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		defer stop()
		if !strings.Contains(logs.String(), "starting as a new registry") {
			t.Fatalf("log does not say what happened:\n%s", logs.String())
		}
	})
}

func TestRestoreFromAFile(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	logs, addr, stop := regtest.RunServe(t, Run, src, map[string]string{"REGISTRY_ADMIN_TOKEN": backupAdmin})
	if addr == "" {
		t.Fatalf("serve did not start:\n%s", logs.String())
	}
	if code, _, errOut := ctl(t, addr, backupAdmin, "push", modelDir(t), "acme/tiny"); code != 0 {
		t.Fatalf("push: %s", errOut)
	}
	saved := filepath.Join(t.TempDir(), "snapshot.db")
	if code, _, errOut := ctl(t, addr, backupAdmin, "backup", "download", saved); code != 0 {
		t.Fatalf("download: %s", errOut)
	}
	// A local-disk registry keeps stored backups in its data directory.
	if code, _, errOut := ctl(t, addr, backupAdmin, "backup", "now"); code != 0 {
		t.Fatalf("backup now on local storage: %s", errOut)
	}
	if entries, err := os.ReadDir(filepath.Join(src, "backups")); err != nil || len(entries) != 1 {
		t.Fatalf("backups directory: %v, %v", entries, err)
	}
	stop()

	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, &stdout, &stderr, func(string) string { return "" })
		return code, stdout.String(), stderr.String()
	}

	target := t.TempDir()
	if code, out, errOut := run("restore", saved, "--data", target); code != 0 || !strings.Contains(out, "1 repositories") {
		t.Fatalf("restore from a file: %d %q %q", code, out, errOut)
	}
	tests := []struct {
		name string
		args []string
		say  string
	}{
		{"neither a file nor --latest", []string{"restore", "--data", t.TempDir()}, "exactly one"},
		{"both a file and --latest", []string{"restore", saved, "--latest", "--data", t.TempDir()}, "exactly one"},
		{"a file that is not a snapshot", []string{"restore", filepath.Join(src, "..", "nope.db"), "--data", t.TempDir()}, "cannot be restored"},
		{"over an existing database", []string{"restore", saved, "--data", target}, "--replace"},
		{"--latest with nothing stored", []string{"restore", "--latest", "--data", t.TempDir()}, "no backups"},
		{"a stored backup that does not exist", []string{"restore", "--name", "registry-19990101T000000Z.db", "--data", src + "-copy"}, "backup not found"},
	}
	for _, tc := range tests {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			code, _, errOut := run(tc.args...)
			if code == 0 || !strings.Contains(errOut, tc.say) {
				t.Fatalf("code %d, stderr %q, want it to mention %q", code, errOut, tc.say)
			}
		})
	}
	if code, out, errOut := run("restore", saved, "--data", target, "--replace"); code != 0 || !strings.Contains(out, "is kept at") {
		t.Fatalf("restore --replace: %d %q %q", code, out, errOut)
	}
}

func TestScheduledBackupsRunAndPrune(t *testing.T) {
	t.Parallel()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The test is the clock: it sends each tick, and it reads each line the
	// loop logs, which is how it knows the loop has finished with that tick.
	// Nothing here waits for time to pass.
	const keep = 2
	tick := make(chan time.Time)
	logged := make(logLines)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		runBackups(ctx, reg, tick, keep, slog.New(logged))
	}()

	seen := map[string]bool{}
	for n := 1; n <= keep+1; n++ {
		tick <- time.Time{}
		if line := <-logged; line.Level != slog.LevelInfo {
			t.Fatalf("tick %d: the loop logged %q at level %s, want the backup it stored", n, line.Message, line.Level)
		}
		list, err := reg.Backups(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The loop is back waiting for a tick, so nothing is in flight and
		// the limit holds exactly.
		if want := min(n, keep); len(list) != want {
			t.Fatalf("after tick %d: %d backups stored, want %d (the limit is %d): %v", n, len(list), want, keep, list)
		}
		newest := list[len(list)-1].Name
		if seen[newest] {
			t.Fatalf("after tick %d: the newest backup is still %s; the tick stored nothing new", n, newest)
		}
		seen[newest] = true
	}

	// The schedule stops when its context does.
	cancel()
	<-finished
}

// logLines is a log handler that hands every record to whoever reads the
// channel, and waits until it is read.
type logLines chan slog.Record

func (l logLines) Enabled(context.Context, slog.Level) bool { return true }
func (l logLines) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l logLines) WithGroup(string) slog.Handler            { return l }
func (l logLines) Handle(_ context.Context, r slog.Record) error {
	l <- r
	return nil
}

func TestBackupSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		env       map[string]string
		wantEvery time.Duration
		wantKeep  int
		wantErr   string
	}{
		{"off by default", nil, 0, 48, ""},
		{"hourly", map[string]string{"REGISTRY_BACKUP_INTERVAL": "1h"}, time.Hour, 48, ""},
		{"with a keep count", map[string]string{"REGISTRY_BACKUP_INTERVAL": "30m", "REGISTRY_BACKUP_KEEP": "10"}, 30 * time.Minute, 10, ""},
		{"keep everything", map[string]string{"REGISTRY_BACKUP_KEEP": "0"}, 0, 0, ""},
		{"too frequent", map[string]string{"REGISTRY_BACKUP_INTERVAL": "5s"}, 0, 0, "at least a minute"},
		{"not a duration", map[string]string{"REGISTRY_BACKUP_INTERVAL": "hourly"}, 0, 0, "such as 1h"},
		{"negative keep", map[string]string{"REGISTRY_BACKUP_KEEP": "-1"}, 0, 0, "REGISTRY_BACKUP_KEEP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &env{getenv: func(k string) string { return tc.env[k] }}
			every, keep, err := backupSettings(e)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || every != tc.wantEvery || keep != tc.wantKeep {
				t.Fatalf("got %v, %d, %v", every, keep, err)
			}
		})
	}
}
