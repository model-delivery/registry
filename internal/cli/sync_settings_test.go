// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"
)

// The sync interval is off unless set, at least five minutes, and a value
// that cannot be read is an error naming it.
func TestSyncSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr string
	}{
		{"off by default", "", 0, ""},
		{"off when 0", "0", 0, ""},
		{"hourly", "1h", time.Hour, ""},
		{"the minimum", "5m", 5 * time.Minute, ""},
		{"too frequent", "1m", 0, "at least five minutes"},
		{"not a duration", "hourly", 0, "REGISTRY_SYNC_INTERVAL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &env{getenv: func(k string) string {
				if k == "REGISTRY_SYNC_INTERVAL" {
					return tc.value
				}
				return ""
			}}
			got, err := syncSettings(e)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

// serve refuses an interval it cannot use, from the variable or the flag,
// with a usage error naming the setting.
func TestServeRefusesAnUnusableSyncInterval(t *testing.T) {
	t.Parallel()
	admin := map[string]string{"REGISTRY_ADMIN_TOKEN": "md_admin_token_for_the_sync_test"}
	tests := []struct {
		name  string
		env   map[string]string
		flags []string
		want  string
	}{
		{"variable too frequent", map[string]string{"REGISTRY_ADMIN_TOKEN": admin["REGISTRY_ADMIN_TOKEN"], "REGISTRY_SYNC_INTERVAL": "30s"}, nil, "REGISTRY_SYNC_INTERVAL"},
		{"flag too frequent", admin, []string{"--sync-interval", "1m"}, "--sync-interval"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, errOut := serveOnce(t, tc.env, tc.flags...)
			if code != 2 || !strings.Contains(errOut, tc.want) {
				t.Fatalf("exit code %d, want 2 with %q:\n%s", code, tc.want, errOut)
			}
		})
	}
}
