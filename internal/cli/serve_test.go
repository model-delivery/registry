// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSizeLimitSettings(t *testing.T) {
	t.Parallel()
	// REGISTRY_MAX_BLOB_BYTES is read the same way.
	const key = "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES"
	tests := []struct {
		value   string
		want    int64
		wantErr bool
	}{
		{"", 0, false}, // unset: no limit, as before the setting existed
		{"0", 0, false},
		{"21474836480", 20 << 30, false},
		{"-1", 0, true},
		{"20GB", 0, true},
		{"1.5", 0, true},
	}
	for _, tc := range tests {
		e := &env{getenv: func(k string) string {
			if k == key {
				return tc.value
			}
			return ""
		}}
		got, err := envBytes(e, key)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%q: got %d, %v; want %d, error %v", tc.value, got, err, tc.want, tc.wantErr)
		}
		if tc.wantErr && (!errors.As(err, new(usageError)) || !strings.Contains(err.Error(), key)) {
			t.Errorf("%q: error %v does not name the setting as a usage error", tc.value, err)
		}
	}
}

// The size limits reach the server from its environment, and a registry
// started without them accepts what it always did.
func TestServeSizeLimits(t *testing.T) {
	t.Parallel()
	const admin = "md_admin_token_for_the_size_limit_test"
	src := modelDir(t) // config.json is 2 bytes, sub/weights.bin is 4096

	t.Run("REGISTRY_MAX_BLOB_BYTES refuses a larger upload", func(t *testing.T) {
		t.Parallel()
		logs, addr, stop := runServe(t, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_MAX_BLOB_BYTES": "1000"})
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		defer stop()
		code, _, errOut := ctl(t, addr, admin, "push", src, "acme/tiny")
		if code != 1 || !strings.Contains(errOut, "REGISTRY_MAX_BLOB_BYTES") {
			t.Fatalf("push of a 4096-byte file: code %d\n%s", code, errOut)
		}
	})

	t.Run("REGISTRY_PULL_THROUGH_MAX_FILE_BYTES refuses a larger fetch", func(t *testing.T) {
		t.Parallel()
		hub := newFixture(t, "")
		if code, _, errOut := hub.run(t, hub.admin, "push", src, "acme/tiny"); code != 0 {
			t.Fatalf("seed the upstream: %s", errOut)
		}
		logs, addr, stop := runServe(t, t.TempDir(), map[string]string{
			"REGISTRY_ADMIN_TOKEN": admin, "REGISTRY_UPSTREAM": hub.url,
			"REGISTRY_PULL_THROUGH": "true", "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES": "1000",
		})
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		defer stop()
		if got := httpStatus(t, addr, "/acme/tiny/resolve/main/config.json", ""); got != http.StatusOK {
			t.Fatalf("a small file: status %d", got)
		}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/acme/tiny/resolve/main/sub/weights.bin", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(msg), "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES") {
			t.Fatalf("a 4096-byte file: status %d: %s", resp.StatusCode, msg)
		}
	})

	t.Run("neither set: no limit", func(t *testing.T) {
		t.Parallel()
		logs, addr, stop := runServe(t, t.TempDir(), map[string]string{"REGISTRY_ADMIN_TOKEN": admin})
		if addr == "" {
			t.Fatalf("serve did not start:\n%s", logs.String())
		}
		defer stop()
		if code, _, errOut := ctl(t, addr, admin, "push", src, "acme/tiny"); code != 0 {
			t.Fatalf("push: code %d\n%s", code, errOut)
		}
	})

	for _, key := range []string{"REGISTRY_MAX_BLOB_BYTES", "REGISTRY_PULL_THROUGH_MAX_FILE_BYTES"} {
		t.Run(key+" that is not a number stops the server from starting", func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"REGISTRY_ADMIN_TOKEN": admin, key: "10GB"}
			var stderr bytes.Buffer
			code := Run(context.Background(), []string{"serve", "--addr", "127.0.0.1:0", "--data", t.TempDir()},
				&bytes.Buffer{}, &stderr, func(k string) string { return env[k] })
			if code != 2 || !strings.Contains(stderr.String(), key) {
				t.Fatalf("code %d, stderr %q", code, stderr.String())
			}
		})
	}
}
