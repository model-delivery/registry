// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/blob/blobtest"
	"github.com/model-delivery/registry/internal/registry"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

type fixture struct {
	url   string
	admin string
	reg   *registry.Registry
	data  string // the registry's data directory
}

func newFixture(t *testing.T, upstream string) *fixture {
	t.Helper()
	in := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: upstream})
	return &fixture{url: in.URL, admin: in.Admin, reg: in.Reg, data: in.Data}
}

// run executes modelctl against the fixture and returns exit code, stdout
// and stderr.
func (f *fixture) run(t *testing.T, token string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := map[string]string{"MODELCTL_ENDPOINT": f.url, "MODELCTL_TOKEN": token}
	code := Run(context.Background(), args, &stdout, &stderr, func(k string) string { return env[k] })
	return code, stdout.String(), stderr.String()
}

func modelDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"config.json": "{}", "sub/weights.bin": strings.Repeat("w", 4096)} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCommandsEndToEnd(t *testing.T) {
	t.Parallel()
	hub := newFixture(t, "")
	f := newFixture(t, hub.url)
	src := modelDir(t)

	// Flags may come before or after the positional arguments.
	code, out, errOut := f.run(t, f.admin, "push", src, "acme/tiny", "--tag", "v1", "--message", "first")
	if code != 0 || !strings.Contains(out, "Pushed acme/tiny at revision ") {
		t.Fatalf("push: code %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "uploaded") {
		t.Errorf("push progress missing: %s", errOut)
	}

	code, out, _ = f.run(t, "", "list")
	if code != 0 || !strings.Contains(out, "acme/tiny") || !strings.Contains(out, "REPOSITORY") {
		t.Fatalf("list: code %d\n%s", code, out)
	}

	code, out, _ = f.run(t, "", "list", "--json")
	var listing struct {
		Repos []struct {
			Name      string `json:"name"`
			Revisions int    `json:"revisions"`
		} `json:"repos"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &listing) != nil || len(listing.Repos) != 1 || listing.Repos[0].Revisions != 1 {
		t.Fatalf("list --json: code %d\n%s", code, out)
	}

	code, out, _ = f.run(t, "", "inspect", "acme/tiny", "--revision", "v1")
	for _, want := range []string{"main", "v1", "tag", "sub/weights.bin", "config.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect output missing %q:\n%s", want, out)
		}
	}
	if code != 0 {
		t.Fatalf("inspect: code %d", code)
	}

	dst := filepath.Join(t.TempDir(), "out")
	code, out, errOut = f.run(t, "", "pull", "--dir", dst, "acme/tiny")
	if code != 0 || !strings.Contains(out, "2 files verified") {
		t.Fatalf("pull: code %d\n%s\n%s", code, out, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dst, "sub", "weights.bin"))
	if err != nil || len(got) != 4096 {
		t.Fatalf("pulled file wrong: %v", err)
	}

	// Mirror from the other registry, which plays the upstream hub.
	if code, _, errOut := hub.run(t, hub.admin, "push", src, "upstream/model"); code != 0 {
		t.Fatalf("seed upstream: %s", errOut)
	}
	code, out, errOut = f.run(t, f.admin, "mirror", "upstream/model")
	if code != 0 || !strings.Contains(out, "Mirrored upstream/model at revision ") || !strings.Contains(out, "2 files verified") {
		t.Fatalf("mirror: code %d\n%s\n%s", code, out, errOut)
	}
	// Same content as acme/tiny, so nothing had to be downloaded.
	if !strings.Contains(out, "0 B downloaded") {
		t.Errorf("mirror re-downloaded content the registry already had:\n%s", out)
	}

	code, out, _ = f.run(t, f.admin, "delete", "acme/tiny", "--yes")
	if code != 0 || !strings.Contains(out, "Deleted acme/tiny") {
		t.Fatalf("delete: code %d\n%s", code, out)
	}
	if _, err := f.reg.GetRepo(context.Background(), "acme/tiny"); err == nil {
		t.Fatal("repository still exists after delete")
	}
}

func TestErrorsAreActionable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	src := modelDir(t)

	tests := []struct {
		name     string
		token    string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no command", "", nil, 2, "modelctl mirror"},
		{"unknown command", "", []string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{"unknown flag", "", []string{"list", "--nope"}, 2, "flag provided but not defined"},
		{"missing argument", "", []string{"pull"}, 2, "expected 1 argument"},
		{"push without token", "", []string{"push", src, "acme/tiny"}, 1, "MODELCTL_TOKEN"},
		{"push of a non-directory", f.admin, []string{"push", filepath.Join(src, "config.json"), "acme/tiny"}, 2, "is not a directory"},
		{"push to a bad name", f.admin, []string{"push", src, "nonamespace"}, 2, "namespace/name"},
		{"pull of unknown repository", "", []string{"pull", "acme/absent"}, 1, "repository not found"},
		{"inspect unknown revision", "", []string{"inspect", "acme/absent", "--revision", "x"}, 1, "repository not found"},
		{"delete needs confirmation", f.admin, []string{"delete", "acme/tiny"}, 2, "--yes"},
		{"delete without admin", "", []string{"delete", "acme/tiny", "--yes"}, 1, "MODELCTL_TOKEN"},
		{"token needs a subcommand", f.admin, []string{"token"}, 2, "create, list or revoke"},
		{"token with a bad role", f.admin, []string{"token", "create", "x", "--role", "root"}, 1, "read, write or admin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := f.run(t, tc.token, tc.args...)
			if code != tc.wantCode || !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("code %d (want %d), stderr:\n%s\nwant it to mention %q", code, tc.wantCode, errOut, tc.wantErr)
			}
		})
	}

	t.Run("unreachable registry", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		env := map[string]string{"MODELCTL_ENDPOINT": "http://127.0.0.1:1"}
		code := Run(context.Background(), []string{"list"}, &stdout, &stderr, func(k string) string { return env[k] })
		if code != 1 || !strings.Contains(stderr.String(), "cannot reach the registry at http://127.0.0.1:1") {
			t.Fatalf("code %d: %s", code, stderr.String())
		}
	})
}

func TestTokenLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	code, out, errOut := f.run(t, f.admin, "token", "create", "ci", "--role", "write")
	secret := strings.TrimSpace(out)
	if code != 0 || !strings.HasPrefix(secret, "md_") {
		t.Fatalf("token create: code %d out %q err %q", code, out, errOut)
	}
	// Only the secret goes to stdout, so it can be captured by a script.
	if strings.ContainsAny(secret, " \n") {
		t.Fatalf("stdout carries more than the token: %q", out)
	}

	if code, _, errOut := f.run(t, secret, "push", modelDir(t), "acme/tiny"); code != 0 {
		t.Fatalf("new write token cannot push: %s", errOut)
	}
	code, out, _ = f.run(t, f.admin, "token", "list")
	if code != 0 || !strings.Contains(out, "ci") || strings.Contains(out, secret) {
		t.Fatalf("token list: code %d\n%s", code, out)
	}
	if code, _, errOut := f.run(t, f.admin, "token", "revoke", "ci"); code != 0 {
		t.Fatalf("token revoke: %s", errOut)
	}
	if code, _, _ := f.run(t, secret, "push", modelDir(t), "acme/other"); code == 0 {
		t.Fatal("revoked token can still push")
	}
}

func TestServeWithS3Storage(t *testing.T) {
	t.Parallel()
	fake := blobtest.NewFakeS3(t)
	const admin = "md_admin_token_for_the_s3_test"
	data := t.TempDir()
	logs, addr, stop := regtest.RunServe(t, Run, data, regtest.S3Env(fake, admin, nil))
	if addr == "" {
		t.Fatalf("serve exited early with code %d:\n%s", stop(), logs.String())
	}

	if code, _, errOut := ctl(t, addr, admin, "push", modelDir(t), "acme/tiny"); code != 0 {
		t.Fatalf("push to an S3-backed registry failed: %s", errOut)
	}
	keys := fake.Keys(t)
	if len(keys) != 2 {
		t.Fatalf("bucket holds %v, want the two pushed files", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, regtest.S3Prefix+"/sha256/") {
			t.Errorf("object %q is not under the configured prefix", k)
		}
	}
	// The files live in the bucket, not in the data directory.
	if _, err := os.Stat(filepath.Join(data, "blobs")); !os.IsNotExist(err) {
		t.Errorf("a local blobs directory exists alongside S3 storage: %v", err)
	}
	if strings.Contains(logs.String(), fake.Config.SecretKey) {
		t.Error("the S3 secret key was written to the log")
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exit code %d:\n%s", code, logs.String())
	}
}

func TestServeStorageConfigurationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"s3 without a bucket", map[string]string{"REGISTRY_STORAGE": "s3"}, "REGISTRY_S3_BUCKET"},
		{"unknown storage", map[string]string{"REGISTRY_STORAGE": "tape"}, "use filesystem or s3"},
		{"malformed pull-through pattern", map[string]string{"REGISTRY_PULL_THROUGH": "true", "REGISTRY_PULL_THROUGH_ALLOW": "meta-llama/*, [bad"}, "is not a valid pattern"},
		{"s3 bucket that does not exist", map[string]string{"REGISTRY_STORAGE": "s3", "REGISTRY_S3_BUCKET": "absent",
			"REGISTRY_S3_ENDPOINT": "FAKE", "REGISTRY_S3_ACCESS_KEY": "k", "REGISTRY_S3_SECRET_KEY": "s"}, `bucket "absent" does not exist`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := tc.env
			if env["REGISTRY_S3_ENDPOINT"] == "FAKE" {
				env["REGISTRY_S3_ENDPOINT"] = blobtest.NewFakeS3(t).Config.Endpoint
			}
			var stderr bytes.Buffer
			code := Run(context.Background(), []string{"serve", "--addr", "127.0.0.1:0", "--data", t.TempDir()},
				&bytes.Buffer{}, &stderr, func(k string) string { return env[k] })
			if code == 0 || !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("code %d, stderr %q, want an error mentioning %q", code, stderr.String(), tc.wantErr)
			}
		})
	}
}

func TestServeFirstStartAndRestart(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	tokenRe := regexp.MustCompile(`md_[0-9a-f]{48}`)

	serve := func(env map[string]string) (logs *regtest.LogBuffer, addr string, stop func() int) {
		logs, addr, stop = regtest.RunServe(t, Run, data, env)
		if addr == "" {
			t.Fatalf("serve exited early with code %d:\n%s", stop(), logs.String())
		}
		return logs, addr, stop
	}
	status := func(addr, path, token string) int {
		return regtest.Status(t, "http://"+addr+path, token)
	}

	// First start mints an admin token and prints it.
	logs, addr, stop := serve(nil)
	token := tokenRe.FindString(logs.String())
	if token == "" {
		t.Fatalf("first start printed no admin token:\n%s", logs.String())
	}
	if got := status(addr, "/healthz", ""); got != 200 {
		t.Fatalf("healthz = %d", got)
	}
	if got := status(addr, "/api/v1/tokens", token); got != 200 {
		t.Fatalf("printed admin token was refused: %d", got)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exit code %d:\n%s", code, logs.String())
	}

	// A restart keeps the token and prints no new one.
	logs, addr, stop = serve(nil)
	if tokenRe.MatchString(logs.String()) {
		t.Fatal("restart printed a token again")
	}
	if got := status(addr, "/api/v1/tokens", token); got != 200 {
		t.Fatalf("admin token did not survive a restart: %d", got)
	}
	stop()

	// An operator-supplied admin token takes effect, and is not logged.
	const supplied = "md_operator_supplied_admin_token"
	logs, addr, stop = serve(map[string]string{"REGISTRY_ADMIN_TOKEN": supplied, "REGISTRY_REQUIRE_AUTH": "true"})
	if strings.Contains(logs.String(), supplied) {
		t.Fatal("supplied admin token was written to the log")
	}
	if got := status(addr, "/api/v1/tokens", supplied); got != 200 {
		t.Fatalf("supplied admin token refused: %d", got)
	}
	if got := status(addr, "/api/v1/repos", ""); got != 401 {
		t.Fatalf("REGISTRY_REQUIRE_AUTH did not close anonymous reads: %d", got)
	}
	stop()
}

// On a platform that collects logs, the admin token is supplied, usually
// from a mounted secret, and must never be generated and printed.
func TestServeAdminTokenForContainerPlatforms(t *testing.T) {
	t.Parallel()
	generated := regexp.MustCompile(`md_[0-9a-f]{48}`)
	const supplied = "md_token_from_a_mounted_secret_file"
	secretFile := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "admin-token")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("token read from a file, as a mounted secret provides it", func(t *testing.T) {
		t.Parallel()
		// Secret files end with a newline; it is not part of the token.
		logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN_FILE": secretFile(t, supplied+"\n")})
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		defer stop()
		if got := regtest.Status(t, "http://"+addr+"/api/v1/tokens", supplied); got != http.StatusOK {
			t.Fatalf("token from the file was refused: %d", got)
		}
		if strings.Contains(logs.String(), supplied) || generated.MatchString(logs.String()) {
			t.Fatalf("a token was written to the log:\n%s", logs.String())
		}
	})

	t.Run("required but not supplied: refuses to start and prints no token", func(t *testing.T) {
		t.Parallel()
		logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), map[string]string{"REGISTRY_REQUIRE_ADMIN_TOKEN": "true"})
		if code := stop(); addr != "" || code == 0 {
			t.Fatalf("serve started (addr %q, exit %d) without an admin token", addr, code)
		}
		if generated.MatchString(logs.String()) {
			t.Fatalf("a token was generated and printed although one was required:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "REGISTRY_ADMIN_TOKEN_FILE") {
			t.Fatalf("the error does not say how to supply one:\n%s", logs.String())
		}
	})

	t.Run("required and supplied: starts", func(t *testing.T) {
		t.Parallel()
		logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), map[string]string{
			"REGISTRY_REQUIRE_ADMIN_TOKEN": "true", "REGISTRY_ADMIN_TOKEN_FILE": secretFile(t, supplied),
		})
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		stop()
	})

	t.Run("required, nothing supplied, but tokens exist from an earlier run: starts", func(t *testing.T) {
		t.Parallel()
		data := t.TempDir()
		_, addr, stop := regtest.RunServe(t, Run, data, map[string]string{"REGISTRY_ADMIN_TOKEN": supplied})
		if addr == "" {
			t.Fatal("first run did not start")
		}
		stop()
		logs, addr, stop := regtest.RunServe(t, Run, data, map[string]string{"REGISTRY_REQUIRE_ADMIN_TOKEN": "true"})
		if addr == "" {
			t.Fatalf("restart refused although an admin token already exists:\n%s", logs.String())
		}
		defer stop()
		if got := regtest.Status(t, "http://"+addr+"/api/v1/tokens", supplied); got != http.StatusOK {
			t.Fatalf("existing admin token refused after restart: %d", got)
		}
	})

	for name, env := range map[string]map[string]string{
		"both the variable and the file": {"REGISTRY_ADMIN_TOKEN": supplied, "REGISTRY_ADMIN_TOKEN_FILE": secretFile(t, supplied)},
		"an empty file":                  {"REGISTRY_ADMIN_TOKEN_FILE": secretFile(t, "\n")},
		"a file that does not exist":     {"REGISTRY_ADMIN_TOKEN_FILE": filepath.Join(t.TempDir(), "absent")},
		"a token that is too short":      {"REGISTRY_ADMIN_TOKEN_FILE": secretFile(t, "short")},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()
			logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), env)
			if code := stop(); addr != "" || code == 0 {
				t.Fatalf("serve started (addr %q, exit %d)", addr, code)
			}
			if strings.Contains(logs.String(), supplied) || generated.MatchString(logs.String()) {
				t.Fatalf("a token was written to the log:\n%s", logs.String())
			}
		})
	}
}
