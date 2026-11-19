// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/regtest"
)

// `modelctl serve` answers GET /metrics with nothing turned on: with the
// version it was built as, and, when the registry requires a token to
// read, only for a request that carries one.
func TestServeAnswersMetrics(t *testing.T) {
	t.Parallel()
	const adminToken = "an-admin-token-long-enough-to-pass-0123456789"
	scrape := func(addr, token string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		text, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(text)
	}
	start := func(env map[string]string) string {
		t.Helper()
		logs, addr, stop := regtest.RunServe(t, Run, t.TempDir(), env)
		if addr == "" {
			t.Fatalf("serve exited early with code %d:\n%s", stop(), logs.String())
		}
		return addr
	}

	t.Run("an open registry", func(t *testing.T) {
		t.Parallel()
		addr := start(map[string]string{"REGISTRY_ADMIN_TOKEN": adminToken})
		status, contentType, text := scrape(addr, "")
		if status != http.StatusOK || !strings.HasPrefix(contentType, "text/plain; version=0.0.4") {
			t.Fatalf("status %d, Content-Type %q", status, contentType)
		}
		for _, line := range []string{
			`modelregistry_build_info{version="` + Version + `"} 1`,
			`modelregistry_storage_blobs{backend="filesystem"} 0`,
			`modelregistry_repositories 0`,
			`modelregistry_backup_last_success_timestamp_seconds 0`,
		} {
			if !strings.Contains(text, line+"\n") {
				t.Errorf("missing %q in:\n%s", line, text)
			}
		}
		if strings.Contains(text, adminToken) {
			t.Error("the metrics contain the admin token")
		}
	})

	t.Run("a registry that requires a token to read", func(t *testing.T) {
		t.Parallel()
		addr := start(map[string]string{"REGISTRY_ADMIN_TOKEN": adminToken, "REGISTRY_REQUIRE_AUTH": "true"})
		if status, _, text := scrape(addr, ""); status != http.StatusUnauthorized || strings.Contains(text, "modelregistry_") {
			t.Fatalf("without a token: status %d: %s", status, text)
		}
		if status, _, _ := scrape(addr, "not-a-token-of-this-registry"); status != http.StatusUnauthorized {
			t.Fatalf("with a token from elsewhere: status %d", status)
		}
		if status, _, text := scrape(addr, adminToken); status != http.StatusOK || !strings.Contains(text, "modelregistry_build_info") {
			t.Fatalf("with a token: status %d", status)
		}
	})
}
