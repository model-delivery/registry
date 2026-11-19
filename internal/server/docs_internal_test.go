// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// apiDocument is docs/api.md, from this package's directory.
const apiDocument = "../../docs/api.md"

var (
	// A route as the document writes one: a method and a path, in backticks.
	documentedRoute = regexp.MustCompile("`((?:GET|HEAD|POST|PUT|DELETE) /[^` ]*)`")
	// A path parameter, whatever it is called: {namespace}, {ns}, {rev...}.
	pathParameter = regexp.MustCompile(`\{[^}]*\}`)
)

// sameRoute removes what differs between a mux pattern and the way the
// document writes the same route, which is only how parameters are named.
func sameRoute(route string) string { return pathParameter.ReplaceAllString(route, "{}") }

// docs/api.md is the reference for the registry's own API: everything
// under /api/v1, and the health check. Every such route the server
// registers is in the document, and every route the document names is
// registered. A route added, renamed or removed fails here until the
// document follows.
//
// The Hugging Face compatible routes are not in that document; they follow
// the Hub's API and docs/compatibility.md describes what is supported.
func TestAPIDocumentMatchesTheRoutes(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(apiDocument)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range documentedRoute.FindAllStringSubmatch(string(raw), -1) {
		documented[sameRoute(m[1])] = true
	}

	s := New(nil, Config{})
	t.Cleanup(s.stop)
	registered := map[string]bool{}
	for _, pattern := range s.patterns {
		_, path, _ := strings.Cut(pattern, " ")
		if strings.HasPrefix(path, "/api/v1/") || path == "/healthz" {
			registered[sameRoute(pattern)] = true
		}
	}
	if len(registered) < 10 {
		t.Fatalf("found only %d routes of the registry's API among %v: this test no longer sees them", len(registered), s.patterns)
	}

	var missing, stale []string
	for route := range registered {
		if !documented[route] {
			missing = append(missing, route)
		}
	}
	for route := range documented {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	slices.Sort(missing)
	slices.Sort(stale)
	if len(missing) > 0 {
		t.Errorf("registered but not in docs/api.md (write each as `METHOD /path` there):\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("named in docs/api.md but not registered:\n  %s", strings.Join(stale, "\n  "))
	}
}

func TestAgo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		before time.Duration
		want   string
	}{
		{0, "just now"},
		{59 * time.Second, "just now"},
		{time.Minute, "1 min ago"},
		{59*time.Minute + 59*time.Second, "59 min ago"},
		{time.Hour, "1 h ago"},
		{47*time.Hour + 59*time.Minute, "47 h ago"},
		{48 * time.Hour, "2 days ago"},
		{30 * 24 * time.Hour, "30 days ago"},
		// A clock that is behind, or a time recorded by another machine: not
		// a negative age.
		{-time.Hour, "just now"},
	}
	for _, tc := range tests {
		if got := ago(now, now.Add(-tc.before)); got != tc.want {
			t.Errorf("%s before now: %q, want %q", tc.before, got, tc.want)
		}
	}
}
