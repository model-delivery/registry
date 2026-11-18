// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/registry"
)

func TestVerifyCommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	if code, _, errOut := f.run(t, f.admin, "push", modelDir(t), "acme/tiny"); code != 0 {
		t.Fatalf("push: %s", errOut)
	}

	t.Run("an intact registry: says so and exits zero", func(t *testing.T) {
		code, out, errOut := f.run(t, f.admin, "verify")
		if code != 0 || !strings.Contains(out, "Verified 2 stored files") || !strings.Contains(out, "every one is present and matches its digest") {
			t.Fatalf("code %d\n%s\n%s", code, out, errOut)
		}
		if !strings.Contains(errOut, "Checking 2 stored files") {
			t.Errorf("no progress line: %s", errOut)
		}
		code, out, _ = f.run(t, f.admin, "verify", "--json")
		var doc struct {
			Blobs    int               `json:"blobs"`
			Failed   int               `json:"failed"`
			Problems []json.RawMessage `json:"problems"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &doc) != nil || doc.Blobs != 2 || doc.Failed != 0 || doc.Problems == nil || len(doc.Problems) != 0 {
			t.Fatalf("--json: code %d\n%s", code, out)
		}
	})

	t.Run("needs the admin role", func(t *testing.T) {
		writer, err := f.reg.CreateToken(context.Background(), "ci", registry.RoleWrite)
		if err != nil {
			t.Fatal(err)
		}
		if code, _, errOut := f.run(t, writer, "verify"); code != 1 || !strings.Contains(errOut, "forbidden") {
			t.Fatalf("write token: code %d: %s", code, errOut)
		}
		if code, _, errOut := f.run(t, "", "verify"); code != 1 || !strings.Contains(errOut, "MODELCTL_TOKEN") {
			t.Fatalf("no token: code %d: %s", code, errOut)
		}
	})

	t.Run("a damaged file: names it and exits non-zero", func(t *testing.T) {
		// Change the stored weights on disk, as a failing disk might.
		weights := strings.Repeat("w", 4096)
		sum := sha256.Sum256([]byte(weights))
		digest := hex.EncodeToString(sum[:])
		stored := filepath.Join(f.data, "blobs", "sha256", digest[:2], digest)
		if err := os.WriteFile(stored, []byte(strings.Repeat("W", 4096)), 0o600); err != nil {
			t.Fatal(err)
		}

		code, out, errOut := f.run(t, f.admin, "verify")
		if code != 1 {
			t.Fatalf("code %d, want 1\n%s\n%s", code, out, errOut)
		}
		for _, want := range []string{"damaged", digest, "used by acme/tiny"} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not mention %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "every one is present") {
			t.Errorf("claims success:\n%s", out)
		}
		if !strings.Contains(errOut, "1 of 2 stored files failed the check") || !strings.Contains(errOut, "docs/operations.md") {
			t.Errorf("the error does not say what to do next: %s", errOut)
		}

		code, out, _ = f.run(t, f.admin, "verify", "--json")
		var doc struct {
			Blobs    int `json:"blobs"`
			Failed   int `json:"failed"`
			Problems []struct {
				SHA256 string   `json:"sha256"`
				Status string   `json:"status"`
				Repos  []string `json:"repos"`
			} `json:"problems"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--json output is not JSON: %v\n%s", err, out)
		}
		if code != 1 || doc.Blobs != 2 || doc.Failed != 1 || len(doc.Problems) != 1 ||
			doc.Problems[0].SHA256 != digest || doc.Problems[0].Status != "damaged" || len(doc.Problems[0].Repos) != 1 {
			t.Fatalf("--json: code %d, %+v", code, doc)
		}
	})
}
