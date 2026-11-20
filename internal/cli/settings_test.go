// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/regtest"
)

// The server's on/off settings that come from the environment.
var booleanSettings = []string{
	"REGISTRY_REQUIRE_AUTH", "REGISTRY_REQUIRE_ADMIN_TOKEN", "REGISTRY_PULL_THROUGH",
	"REGISTRY_S3_REDIRECT", "REGISTRY_RESTORE_IF_EMPTY", "REGISTRY_PULL_THROUGH_STREAM",
}

// A setting that is on unless turned off reads the same spellings, and is
// on when its variable is unset or empty.
func TestEnvBoolOr(t *testing.T) {
	t.Parallel()
	const key = "REGISTRY_PULL_THROUGH_STREAM"
	tests := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{"", true, false}, {"   ", true, false},
		{"true", true, false}, {"1", true, false}, {"on", true, false}, {"yes", true, false},
		{"false", false, false}, {"0", false, false}, {"off", false, false}, {"No", false, false},
		// Not read as on, and not as off: refused.
		{"disabled", false, true}, {"none", false, true},
	}
	for _, tc := range tests {
		e := &env{getenv: func(string) string { return tc.value }}
		got, err := envBoolOr(e, key, true)
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("%q: got %v, %v; want %v, error %v", tc.value, got, err, tc.want, tc.wantErr)
		}
		if err != nil && (!errors.As(err, new(usageError)) || !strings.Contains(err.Error(), key)) {
			t.Errorf("%q: %v is not a usage error naming %s", tc.value, err, key)
		}
	}
}

// Sending a file while pull-through fetches it is on unless it is turned
// off, by the flag or by the variable, and the flag wins. The server says
// which at start.
func TestServePullThroughStreamSetting(t *testing.T) {
	t.Parallel()
	const admin = "md_admin_token_for_the_stream_test"
	tests := []struct {
		name  string
		env   map[string]string
		flags []string
		want  string
	}{
		{"on by default", nil, []string{"--pull-through"}, "pull_through_stream=true"},
		{"off by the flag", nil, []string{"--pull-through", "--pull-through-stream=false"}, "pull_through_stream=false"},
		{"off by the variable", map[string]string{"REGISTRY_PULL_THROUGH_STREAM": "off"}, []string{"--pull-through"}, "pull_through_stream=false"},
		{"the flag wins over the variable", map[string]string{"REGISTRY_PULL_THROUGH_STREAM": "false"}, []string{"--pull-through", "--pull-through-stream"}, "pull_through_stream=true"},
		{"nothing to stream without pull-through", nil, nil, "pull_through_stream=false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_REQUIRE_AUTH": "true"}
			for k, v := range tc.env {
				env[k] = v
			}
			logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), env, tc.flags...)
			if addr == "" {
				t.Fatalf("serve did not start:\n%s", logs.String())
			}
			if !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("the server did not start with %s:\n%s", tc.want, logs.String())
			}
		})
	}
}

func TestEnvBool(t *testing.T) {
	t.Parallel()
	const key = "REGISTRY_REQUIRE_AUTH"
	tests := []struct {
		value   string
		want    bool
		wantErr bool
	}{
		{"", false, false}, // unset: off, as it always was
		// What strconv.ParseBool reads, which is what was read before.
		{"1", true, false}, {"t", true, false}, {"T", true, false},
		{"true", true, false}, {"TRUE", true, false}, {"True", true, false},
		{"0", false, false}, {"f", false, false}, {"F", false, false},
		{"false", false, false}, {"FALSE", false, false}, {"False", false, false},
		// What people also write. These used to mean off, without a word.
		{"yes", true, false}, {"YES", true, false}, {"Yes", true, false},
		{"on", true, false}, {"ON", true, false}, {"On", true, false},
		{"no", false, false}, {"NO", false, false}, {"No", false, false},
		{"off", false, false}, {"OFF", false, false}, {"Off", false, false},
		// Space around a value, as an env file leaves it, is not part of it.
		{" true ", true, false}, {"yes\r", true, false}, {"   ", false, false},
		// Anything else is refused, not read as off.
		{"enabled", false, true}, {"y", false, true}, {"n", false, true},
		{"2", false, true}, {"-1", false, true}, {"tru", false, true},
		{"yes please", false, true}, {"true,false", false, true}, {"oui", false, true},
	}
	for _, tc := range tests {
		e := &env{getenv: func(k string) string {
			if k == key {
				return tc.value
			}
			return ""
		}}
		got, err := envBool(e, key)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%q: got %v, %v; want %v, error %v", tc.value, got, err, tc.want, tc.wantErr)
		}
		if !tc.wantErr {
			continue
		}
		// A usage error (exit code 2) that names the variable, what it was
		// set to, and what would be accepted.
		if !errors.As(err, new(usageError)) {
			t.Errorf("%q: %v is not a usage error", tc.value, err)
		}
		for _, want := range []string{key, tc.value, "true or false", "yes", "no", "on", "off"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: the error does not mention %q: %v", tc.value, want, err)
			}
		}
	}
}

// serveOnce runs `modelctl serve` to the point where it would start, with
// a configuration that is expected to stop it, and returns the exit code
// and what it wrote.
func serveOnce(t *testing.T, env map[string]string, flags ...string) (code int, stderr string) {
	t.Helper()
	// A serve that does start, where the test expected it not to, is
	// stopped rather than left running.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errOut bytes.Buffer
	args := append([]string{"serve", "--addr", "127.0.0.1:0", "--data", t.TempDir()}, flags...)
	code = Run(ctx, args, &bytes.Buffer{}, &errOut, func(k string) string { return env[k] })
	return code, errOut.String()
}

// A value the server cannot read for an on/off setting stops it from
// starting. It used to be read as off: REGISTRY_REQUIRE_AUTH=enabled
// started a registry open to anyone, with no warning.
func TestServeRefusesAnUnreadableBoolean(t *testing.T) {
	t.Parallel()
	for _, key := range booleanSettings {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			code, errOut := serveOnce(t, map[string]string{"REGISTRY_ADMIN_TOKEN": "md_admin_token_for_the_boolean_test", key: "enabled"})
			if code != 2 {
				t.Fatalf("exit code %d, want 2 (a usage error):\n%s", code, errOut)
			}
			for _, want := range []string{key, `"enabled"`, "true or false"} {
				if !strings.Contains(errOut, want) {
					t.Errorf("the error does not mention %s:\n%s", want, errOut)
				}
			}
			if strings.Contains(errOut, "registry listening") {
				t.Fatalf("the server started:\n%s", errOut)
			}
		})
	}

	t.Run("--help still works with one set wrongly", func(t *testing.T) {
		t.Parallel()
		code, errOut := serveOnce(t, map[string]string{"REGISTRY_REQUIRE_AUTH": "enabled"}, "--help")
		if code != 0 || !strings.Contains(errOut, "--require-auth") && !strings.Contains(errOut, "-require-auth") {
			t.Fatalf("code %d:\n%s", code, errOut)
		}
	})
}

// "yes" and "on" now mean what they say.
func TestServeReadsYesAndOn(t *testing.T) {
	t.Parallel()
	const admin = "md_admin_token_for_the_spelling_test"
	tests := []struct {
		value string
		want  int // an anonymous read
	}{
		{"yes", http.StatusUnauthorized},
		{"ON", http.StatusUnauthorized},
		{"true", http.StatusUnauthorized},
		{"no", http.StatusOK},
		{"off", http.StatusOK},
		{"", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run("REGISTRY_REQUIRE_AUTH="+tc.value, func(t *testing.T) {
			t.Parallel()
			logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_REQUIRE_AUTH": tc.value})
			if addr == "" {
				t.Fatalf("serve did not start:\n%s", logs.String())
			}
			if got := regtest.Status(t, "http://"+addr+"/api/v1/repos", ""); got != tc.want {
				t.Fatalf("an anonymous read: status %d, want %d", got, tc.want)
			}
		})
	}
}

// Every setting that is not a secret can be given as a flag, and --help
// shows the flag beside the variable. The secrets have no flag, and --help
// says why.
func TestServeHelpListsEverySetting(t *testing.T) {
	t.Parallel()
	code, help := serveOnce(t, nil, "--help")
	if code != 0 {
		t.Fatalf("code %d:\n%s", code, help)
	}
	settings := map[string]string{
		"addr":                        "REGISTRY_ADDR",
		"data":                        "REGISTRY_DATA",
		"upstream":                    "REGISTRY_UPSTREAM",
		"require-auth":                "REGISTRY_REQUIRE_AUTH",
		"require-admin-token":         "REGISTRY_REQUIRE_ADMIN_TOKEN",
		"pull-through":                "REGISTRY_PULL_THROUGH",
		"pull-through-allow":          "REGISTRY_PULL_THROUGH_ALLOW",
		"pull-through-max-file-bytes": "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES",
		"pull-through-stream":         "REGISTRY_PULL_THROUGH_STREAM",
		"max-blob-bytes":              "REGISTRY_MAX_BLOB_BYTES",
		"backup-interval":             "REGISTRY_BACKUP_INTERVAL",
		"backup-keep":                 "REGISTRY_BACKUP_KEEP",
		"sync-interval":               "REGISTRY_SYNC_INTERVAL",
		"restore-if-empty":            "REGISTRY_RESTORE_IF_EMPTY",
		"storage":                     "REGISTRY_STORAGE",
		"s3-bucket":                   "REGISTRY_S3_BUCKET",
		"s3-endpoint":                 "REGISTRY_S3_ENDPOINT",
		"s3-region":                   "REGISTRY_S3_REGION",
		"s3-prefix":                   "REGISTRY_S3_PREFIX",
		"s3-redirect":                 "REGISTRY_S3_REDIRECT",
	}
	for flagName, variable := range settings {
		// The flag's own entry: its name at the start of a line, and the
		// variable in the text that follows it.
		_, entry, found := strings.Cut(help, "\n  -"+flagName)
		if !found {
			t.Errorf("--help has no flag --%s", flagName)
			continue
		}
		if next := strings.Index(entry, "\n  -"); next >= 0 {
			entry = entry[:next]
		}
		if !strings.Contains(entry, "env "+variable) {
			t.Errorf("--%s does not name its variable %s:\n%s", flagName, variable, entry)
		}
	}
	for _, secret := range []string{"REGISTRY_ADMIN_TOKEN", "REGISTRY_ADMIN_TOKEN_FILE", "REGISTRY_UPSTREAM_TOKEN", "REGISTRY_S3_ACCESS_KEY", "REGISTRY_S3_SECRET_KEY"} {
		if !strings.Contains(help, secret) {
			t.Errorf("--help does not mention %s", secret)
		}
		if strings.Contains(help, "\n  -"+strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(secret, "REGISTRY_"), "_", "-"))) {
			t.Errorf("%s has a flag: secrets must not be given on a command line", secret)
		}
	}
	if !strings.Contains(help, "environment only") || !strings.Contains(help, "process list") {
		t.Errorf("--help does not say why secrets have no flag:\n%s", help)
	}
}

func TestServeFlags(t *testing.T) {
	t.Parallel()
	const admin = "md_admin_token_for_the_flags_test"
	src := modelDir(t) // config.json is 2 bytes, sub/weights.bin is 4096

	t.Run("storage, backups and restore, by flag alone", func(t *testing.T) {
		t.Parallel()
		fake := blobtest.NewFakeS3(t)
		// Only the secrets are in the environment, with one variable that
		// the flag for the same setting must win over.
		env := map[string]string{
			"REGISTRY_ADMIN_TOKEN":   admin,
			"REGISTRY_S3_ACCESS_KEY": fake.Config.AccessKey,
			"REGISTRY_S3_SECRET_KEY": fake.Config.SecretKey,
			"REGISTRY_S3_PREFIX":     "from-the-environment",
			"REGISTRY_BACKUP_KEEP":   "7",
		}
		data := t.TempDir()
		logs, addr, stop := regtest.RunServe(t, Run, data, env,
			"--storage", "s3", "--s3-bucket", fake.Config.Bucket, "--s3-endpoint", fake.Config.Endpoint,
			"--s3-region", fake.Config.Region, "--s3-prefix", "from-the-flag", "--s3-redirect",
			"--restore-if-empty", "--backup-interval", "2h", "--backup-keep", "5")
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		for _, want := range []string{
			"storing model files in S3", "bucket=" + fake.Config.Bucket, "prefix=from-the-flag", "redirect_downloads=true",
			"no database and no stored backup: starting as a new registry", // --restore-if-empty looked
			"scheduled metadata backups are on", "every=2h0m0s", "keep=5",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("the log lacks %q:\n%s", want, logs.String())
			}
		}
		if code, _, errOut := ctl(t, addr, admin, "push", src, "acme/tiny"); code != 0 {
			t.Fatalf("push: %s", errOut)
		}
		for _, k := range fake.Keys(t) {
			if !strings.HasPrefix(k, "from-the-flag/sha256/") {
				t.Errorf("object %q is not under the prefix the flag gave", k)
			}
		}
		// --s3-redirect: a download is answered with a redirect to the bucket.
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/acme/tiny/resolve/main/config.json", nil)
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), fake.Config.Endpoint) {
			t.Errorf("a download: status %d, Location %q; want a redirect to the bucket", resp.StatusCode, resp.Header.Get("Location"))
		}
		// A backup taken through the API is kept by the same count.
		if code, _, errOut := ctl(t, addr, admin, "backup", "now"); code != 0 {
			t.Fatalf("backup now: %s", errOut)
		}
		if code := stop(); code != 0 {
			t.Fatalf("serve exit code %d:\n%s", code, logs.String())
		}

		// restore takes the same storage flags.
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{"restore", "--latest", "--data", t.TempDir(),
			"--storage", "s3", "--s3-bucket", fake.Config.Bucket, "--s3-endpoint", fake.Config.Endpoint,
			"--s3-region", fake.Config.Region, "--s3-prefix", "from-the-flag"},
			&out, &errOut, func(k string) string { return env[k] })
		if code != 0 || !strings.Contains(out.String(), "1 repositories, 1 revisions") {
			t.Fatalf("restore by flags: code %d %q %q", code, out.String(), errOut.String())
		}
	})

	t.Run("--max-blob-bytes refuses a larger upload", func(t *testing.T) {
		t.Parallel()
		logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN": admin}, "--max-blob-bytes", "1000")
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		if code, _, errOut := ctl(t, addr, admin, "push", src, "acme/tiny"); code != 1 || !strings.Contains(errOut, "REGISTRY_MAX_BLOB_BYTES") {
			t.Fatalf("push of a 4096-byte file: code %d\n%s", code, errOut)
		}
	})

	t.Run("pull-through, its allow list and its file limit", func(t *testing.T) {
		t.Parallel()
		hub := newFixture(t, "")
		for _, repo := range []string{"acme/tiny", "other/tiny"} {
			if code, _, errOut := hub.run(t, hub.admin, "push", src, repo); code != 0 {
				t.Fatalf("seed the upstream: %s", errOut)
			}
		}
		logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN": admin},
			"--upstream", hub.url, "--pull-through", "--pull-through-allow", "nobody/*, acme/*", "--pull-through-max-file-bytes", "1000")
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		for path, want := range map[string]int{
			"/acme/tiny/resolve/main/config.json":      http.StatusOK,        // allowed, and small
			"/acme/tiny/resolve/main/sub/weights.bin":  http.StatusForbidden, // allowed, and over the limit
			"/other/tiny/resolve/main/config.json":     http.StatusNotFound,  // not on the allow list
			"/api/models/other/tiny/revision/main":     http.StatusNotFound,
			"/api/v1/repos/acme/tiny/revisions/main":   http.StatusOK,
			"/api/v1/repos/nobody/tiny/revisions/main": http.StatusNotFound, // allowed, and not upstream
		} {
			if got := regtest.Status(t, "http://"+addr+path, ""); got != want {
				t.Errorf("%s: status %d, want %d", path, got, want)
			}
		}
		// The warning about an open registry with no allow list is for a
		// registry with no allow list.
		if strings.Contains(logs.String(), "no allow list") {
			t.Errorf("warned about a missing allow list although --pull-through-allow was given:\n%s", logs.String())
		}
	})

	t.Run("a flag wins over its variable", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{
			"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_REQUIRE_AUTH": "true", "REGISTRY_MAX_BLOB_BYTES": "1000",
			"REGISTRY_BACKUP_INTERVAL": "1h",
		}
		logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), env, "--require-auth=false", "--max-blob-bytes", "0", "--backup-interval", "0")
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		if got := regtest.Status(t, "http://"+addr+"/api/v1/repos", ""); got != http.StatusOK {
			t.Errorf("--require-auth=false over REGISTRY_REQUIRE_AUTH=true: an anonymous read is %d", got)
		}
		if code, _, errOut := ctl(t, addr, admin, "push", src, "acme/tiny"); code != 0 {
			t.Errorf("--max-blob-bytes 0 over REGISTRY_MAX_BLOB_BYTES=1000: push refused: %s", errOut)
		}
		if strings.Contains(logs.String(), "scheduled metadata backups are on") {
			t.Errorf("--backup-interval 0 over REGISTRY_BACKUP_INTERVAL=1h: backups are scheduled:\n%s", logs.String())
		}
	})

	t.Run("the variables alone still configure it", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_BACKUP_INTERVAL": "90m", "REGISTRY_BACKUP_KEEP": "3"}
		logs, addr, _ := regtest.RunServe(t, Run, t.TempDir(), env)
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		if got := logs.String(); !strings.Contains(got, "every=1h30m0s") || !strings.Contains(got, "keep=3") {
			t.Fatalf("the schedule from the environment was not applied:\n%s", got)
		}
	})

	// A value a flag cannot take is a usage error that names the flag, as a
	// bad value in the environment names the variable.
	refused := []struct {
		name    string
		flags   []string
		wantErr string
	}{
		{"an interval under a minute", []string{"--backup-interval", "5s"}, "--backup-interval must be a duration of at least a minute"},
		{"a negative interval", []string{"--backup-interval", "-1h"}, "--backup-interval must be a duration of at least a minute"},
		{"an interval that is not a duration", []string{"--backup-interval", "hourly"}, "backup-interval"},
		{"a negative number to keep", []string{"--backup-keep", "-1"}, "--backup-keep must be a number"},
		{"a negative upload limit", []string{"--max-blob-bytes", "-1"}, "--max-blob-bytes must be a whole number of bytes"},
		{"an upload limit with a unit", []string{"--max-blob-bytes", "20GB"}, "max-blob-bytes"},
		{"a negative pull-through limit", []string{"--pull-through-max-file-bytes", "-5"}, "--pull-through-max-file-bytes must be a whole number of bytes"},
		{"a malformed allow pattern", []string{"--pull-through", "--pull-through-allow", "meta-llama/*,[bad"}, `--pull-through-allow): "[bad" is not a valid pattern`},
		{"s3 without a bucket", []string{"--storage", "s3"}, "--s3-bucket"},
		{"a boolean flag given a word", []string{"--require-auth=enabled"}, "require-auth"},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			code, errOut := serveOnce(t, map[string]string{"REGISTRY_ADMIN_TOKEN": admin}, tc.flags...)
			if code != 2 || !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("code %d, want 2 and an error mentioning %q:\n%s", code, tc.wantErr, errOut)
			}
			if strings.Contains(errOut, "registry listening") {
				t.Fatalf("the server started:\n%s", errOut)
			}
		})
	}
}
