// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// root reads a model at /{ns}/{name}/... and a dataset at
// /datasets/{ns}/{name}/..., as the Hub lays them out. A model whose
// namespace is literally "datasets", recorded before that name was
// reserved, keeps its own addresses wherever they do not also read as a
// dataset's.
func TestParseRootPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		kind rootKind
		key  string
		rest string
	}{
		{"", rootHome, "", ""},
		{"acme/tiny/resolve/main/config.json", rootResolve, "acme/tiny", "main/config.json"},
		{"datasets/acme/tiny/resolve/main/data/train.csv", rootResolve, "datasets/acme/tiny", "main/data/train.csv"},
		{"acme/tiny.git/info/lfs/objects/batch", rootLFSBatch, "acme/tiny", ""},
		{"datasets/acme/tiny.git/info/lfs/objects/batch", rootLFSBatch, "datasets/acme/tiny", ""},
		{"acme/tiny", rootRepoLink, "acme/tiny", ""},
		{"datasets/acme/tiny", rootRepoLink, "datasets/acme/tiny", ""},
		// The model datasets/legacy.
		{"datasets/legacy", rootRepoLink, "datasets/legacy", ""},
		{"datasets/legacy/resolve/main/config.json", rootResolve, "datasets/legacy", "main/config.json"},
		{"datasets/legacy.git/info/lfs/objects/batch", rootLFSBatch, "datasets/legacy", ""},
		// Where both readings fit, the dataset's wins: this is the dataset
		// legacy/resolve's file x at main, not the model datasets/legacy's
		// file main/x at a revision called resolve.
		{"datasets/legacy/resolve/resolve/main/x", rootResolve, "datasets/legacy/resolve", "main/x"},
		// Too short to be a dataset's file: the model's file x at revision
		// resolve.
		{"datasets/legacy/resolve/resolve/x", rootResolve, "datasets/legacy", "resolve/x"},
		{"acme/tiny/blob/main/x", rootOther, "", ""},
		{"datasets/acme/tiny/blob/main/x", rootOther, "", ""},
		{"info", rootOther, "", ""},
	}
	for _, tc := range tests {
		var segs []string
		if tc.path != "" {
			segs = strings.Split(tc.path, "/")
		}
		got := parseRootPath(segs)
		if got.kind != tc.kind {
			t.Errorf("%q is %d, want %d", tc.path, got.kind, tc.kind)
			continue
		}
		if tc.key != "" && got.key() != tc.key {
			t.Errorf("%q addresses %q, want %q", tc.path, got.key(), tc.key)
		}
		if strings.Join(got.rest, "/") != tc.rest {
			t.Errorf("%q leaves %q, want %q", tc.path, strings.Join(got.rest, "/"), tc.rest)
		}
	}
}

// Messages that send someone to `modelctl mirror` name the command that
// fetches that repository.
func TestMirrorCommand(t *testing.T) {
	t.Parallel()
	if got := mirrorCommand("acme/tiny"); got != "modelctl mirror acme/tiny" {
		t.Errorf("for a model: %q", got)
	}
	if got := mirrorCommand("datasets/acme/tiny"); got != "modelctl mirror --repo-type dataset acme/tiny" {
		t.Errorf("for a dataset: %q", got)
	}
}
