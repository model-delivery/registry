// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/model-delivery/registry/internal/server"
)

// syncBuffer is a log destination several requests can write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// When the database cannot be asked whether a token is good, the answer is
// the server's failure (500, logged), not "your token is bad" (401) to
// every user with nothing in the log.
func TestTokenLookupFailureIsAServerError(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	in := start(t, server.Config{AnonymousRead: true, Logger: slog.New(slog.NewTextHandler(logs, nil))})

	// Before: a token the registry does not know is simply not a token.
	resp := do(t, http.MethodGet, in.url+"/api/v1/repos", "hf_some_other_services_token", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an unknown token on an open registry: status %d, want it treated as anonymous", resp.StatusCode)
	}
	resp = do(t, http.MethodPost, in.url+"/api/v1/gc", "hf_some_other_services_token", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unknown token on an admin route: status %d, want 401", resp.StatusCode)
	}

	// The database goes away.
	_ = in.reg.Close()

	tests := []struct {
		name, method, path, token string
	}{
		{"registry API, good token", http.MethodGet, "/api/v1/repos", in.admin},
		{"registry API, unknown token", http.MethodGet, "/api/v1/repos", "hf_some_other_services_token"},
		{"admin route", http.MethodPost, "/api/v1/gc", in.admin},
		{"Hugging Face route", http.MethodGet, "/api/models/acme/tiny", in.read},
		{"whoami", http.MethodGet, "/api/whoami-v2", in.write},
		{"file download", http.MethodGet, "/acme/tiny/resolve/main/config.json", in.read},
	}
	for _, tc := range tests {
		resp := do(t, tc.method, in.url+tc.path, tc.token, nil, nil)
		got := body(t, resp)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500: %s", tc.name, resp.StatusCode, got)
		}
		if resp.Header.Get("WWW-Authenticate") != "" || strings.Contains(got, "token") {
			t.Errorf("%s: the answer blames the token: %s", tc.name, got)
		}
		if strings.Contains(got, "sql") || strings.Contains(got, "database") {
			t.Errorf("%s: the answer carries the internal error: %s", tc.name, got)
		}
	}
	if got := logs.String(); !strings.Contains(got, "look up the request's access token") || !strings.Contains(got, "level=ERROR") {
		t.Fatalf("the failure was not logged as an error:\n%s", got)
	}

	// A request that presents no token asks the database nothing about
	// tokens, so the health check still answers.
	resp = do(t, http.MethodGet, in.url+"/healthz", "", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz without a token: status %d", resp.StatusCode)
	}

	// Signing in to the web UI asks about the token typed into the form.
	form := url.Values{"token": {in.admin}}
	resp = do(t, http.MethodPost, in.url+"/ui/login", "", io.NopCloser(strings.NewReader(form.Encode())),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin"})
	page := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(page, "not valid for this registry") {
		t.Fatalf("sign-in with the database gone: status %d, want 500 and no \"token is not valid\": %s", resp.StatusCode, page)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("sign-in set a cookie although the token could not be checked")
	}
}
