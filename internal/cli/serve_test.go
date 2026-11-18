// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"
)

func TestSizeLimitSettings(t *testing.T) {
	t.Parallel()
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
