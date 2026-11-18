// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The body deadline must be gone once the body has been read, or it would
// cancel a handler that then works for longer than the deadline. This
// drives the helper with a handler of its own, to cover a request with no
// body at all, which no slow route accepts today.
func TestBodyDeadlineIsLiftedOnceTheBodyIsRead(t *testing.T) {
	t.Parallel()
	s := &Server{cfg: Config{BodyTimeout: 100 * time.Millisecond}, log: slog.New(slog.DiscardHandler)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var doc struct{}
		_ = s.readJSON(w, r, 1<<20, &doc, false) // an empty body is an error here, and still fully read
		select {
		case <-r.Context().Done():
			http.Error(w, "the request was cancelled while the handler worked", http.StatusInternalServerError)
		case <-time.After(5 * s.bodyTimeout()):
			_, _ = io.WriteString(w, "finished")
		}
	}))
	t.Cleanup(srv.Close)

	for name, body := range map[string]string{"a body": `{}`, "no body": "", "a body with more after the JSON": `{}   trailing`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var rd io.Reader
			if body != "" {
				rd = strings.NewReader(body)
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, rd)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || string(got) != "finished" {
				t.Fatalf("status %d: %s", resp.StatusCode, got)
			}
		})
	}
}
