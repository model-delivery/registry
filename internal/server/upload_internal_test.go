// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/model-delivery/registry/internal/registry"
)

// The batch route never signs a URL for a file over the size limit, so the
// only way to present one is to hold the signing key. These tests do, to
// check that the upload route enforces the limit by itself.

func TestSignedUploadEnforcesTheSizeLimit(t *testing.T) {
	t.Parallel()
	reg, err := registry.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := &Server{reg: reg, cfg: Config{MaxBlobBytes: 100}, log: slog.New(slog.DiscardHandler), uploadKey: newUploadKey()}

	content := strings.Repeat("x", 101)
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	expires := time.Now().Add(time.Hour).Unix()
	target := fmt.Sprintf("/api/lfs/upload/%s?size=%d&expires=%d&signature=%s", oid, len(content), expires, s.signUpload(oid, int64(len(content)), expires))
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(content))
	req.SetPathValue("sha256", oid)
	rec := httptest.NewRecorder()
	s.hfLFSUpload(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "REGISTRY_MAX_BLOB_BYTES") {
		t.Fatalf("status %d, want 413 naming the setting: %s", rec.Code, rec.Body.String())
	}
	if _, ok, _ := reg.StatBlob(req.Context(), oid); ok {
		t.Fatal("a file over the limit was stored")
	}
}
