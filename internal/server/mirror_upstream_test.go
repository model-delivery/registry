// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/model-delivery/registry/internal/client"
	"github.com/model-delivery/registry/internal/regtest"
	"github.com/model-delivery/registry/internal/server"
)

// A mirror request whose upstream fails before anything is streamed is
// answered like a pull-through lookup in the same situation: 502, with the
// reason. It used to be 500 "internal error", which blamed the registry
// and told the caller nothing. The statuses a refusal already had are
// unchanged.
func TestMirrorAnswersForAFailingUpstream(t *testing.T) {
	t.Parallel()
	const whatTheUpstreamSaid = "stack trace from the hub's own database"

	answering := func(t *testing.T, handler http.HandlerFunc) string {
		t.Helper()
		hub := httptest.NewServer(handler)
		t.Cleanup(hub.Close)
		return hub.URL
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	unreachable := gone.URL
	gone.Close()

	tests := []struct {
		name       string
		upstream   string
		wantStatus int
		wantSays   string
	}{
		{"it cannot be reached", unreachable, http.StatusBadGateway, "could not fetch from upstream"},
		{"it answers with a server error", answering(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, whatTheUpstreamSaid, http.StatusInternalServerError)
		}), http.StatusBadGateway, "could not fetch from upstream"},
		{"its answer is not what a hub sends", answering(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>a proxy's sign-in page</html>"))
		}), http.StatusBadGateway, "could not fetch from upstream"},
		{"it does not have the repository", answering(t, http.NotFound), http.StatusNotFound, "not found upstream"},
		{"it denies access", answering(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, whatTheUpstreamSaid, http.StatusUnauthorized)
		}), http.StatusBadGateway, "upstream denied access"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := regtest.Start(t, server.Config{AnonymousRead: true, Upstream: tc.upstream})
			resp := do(t, http.MethodPost, in.URL+"/api/v1/mirror", in.Write, jsonBody(t, map[string]any{"repo": "acme/tiny"}), nil)
			var answer struct {
				Error string `json:"error"`
			}
			raw := body(t, resp)
			if err := json.Unmarshal([]byte(raw), &answer); err != nil {
				t.Fatalf("status %d with a body that is not the API's error document: %s", resp.StatusCode, raw)
			}
			if resp.StatusCode != tc.wantStatus || !strings.Contains(answer.Error, tc.wantSays) {
				t.Fatalf("status %d %q, want %d and a message saying %q", resp.StatusCode, answer.Error, tc.wantStatus, tc.wantSays)
			}
			if answer.Error == "internal error" {
				t.Fatal("the upstream's failure was reported as the registry's own")
			}
			// What the upstream wrote in its error answer stays in the log.
			if strings.Contains(raw, whatTheUpstreamSaid) {
				t.Fatalf("the answer repeats the upstream's response body: %s", raw)
			}
			// Nothing was recorded.
			if repos, err := in.Reg.ListRepos(context.Background()); err != nil || len(repos) != 0 {
				t.Fatalf("repositories after a failed mirror: %v, %v", repos, err)
			}

			// And what `modelctl mirror` shows is that message, not a bare status.
			_, err := client.New(in.URL, in.Write).Mirror(context.Background(), "acme/tiny", "main", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSays) {
				t.Fatalf("the client reports %v, want it to say %q", err, tc.wantSays)
			}
		})
	}
}
