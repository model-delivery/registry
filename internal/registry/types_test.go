// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRoleAllows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		have, need Role
		want       bool
	}{
		{RoleRead, RoleRead, true},
		{RoleRead, RoleWrite, false},
		{RoleRead, RoleAdmin, false},
		{RoleWrite, RoleRead, true},
		{RoleWrite, RoleWrite, true},
		{RoleWrite, RoleAdmin, false},
		{RoleAdmin, RoleRead, true},
		{RoleAdmin, RoleWrite, true},
		{RoleAdmin, RoleAdmin, true},
		// No role allows nothing, and nothing is allowed by asking for no role.
		{"", RoleRead, false},
		{RoleAdmin, "", false},
		{"", "", false},
		// A name that is not a role is not one, whatever it resembles.
		{"owner", RoleRead, false},
		{"Admin", RoleRead, false},
		{RoleAdmin, "root", false},
	}
	for _, tc := range tests {
		if got := tc.have.Allows(tc.need); got != tc.want {
			t.Errorf("Role(%q).Allows(%q) = %v, want %v", tc.have, tc.need, got, tc.want)
		}
	}
	for _, r := range []Role{RoleRead, RoleWrite, RoleAdmin} {
		if !r.Valid() {
			t.Errorf("%q is not valid", r)
		}
	}
	for _, r := range []Role{"", "owner", "READ", " read"} {
		if r.Valid() {
			t.Errorf("%q is valid", r)
		}
	}
}

// Roles, sources and ref kinds are types in Go and plain strings on the
// wire and in the database, with the values they have always had.
func TestTypedNamesKeepTheirWireValues(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"token", Token{Name: "ci", Role: RoleWrite, CreatedAt: at},
			`{"name":"ci","role":"write","created_at":"2026-11-18T12:00:00Z"}`},
		{"read role", Token{Role: RoleRead, CreatedAt: at}, `{"name":"","role":"read","created_at":"2026-11-18T12:00:00Z"}`},
		{"admin role", Token{Role: RoleAdmin, CreatedAt: at}, `{"name":"","role":"admin","created_at":"2026-11-18T12:00:00Z"}`},
		{"pushed revision", Commit{Repo: "acme/m", SHA: "abc", Source: SourcePush, CreatedAt: at},
			`{"repo":"acme/m","sha":"abc","source":"push","created_at":"2026-11-18T12:00:00Z"}`},
		{"mirrored revision", Commit{Repo: "acme/m", SHA: "abc", Source: SourceMirror, CreatedAt: at},
			`{"repo":"acme/m","sha":"abc","source":"mirror","created_at":"2026-11-18T12:00:00Z"}`},
		{"branch", Ref{Name: "main", Kind: KindBranch, SHA: "abc", UpdatedAt: at},
			`{"name":"main","kind":"branch","sha":"abc","updated_at":"2026-11-18T12:00:00Z"}`},
		{"tag", Ref{Name: "v1", Kind: KindTag, SHA: "abc", UpdatedAt: at},
			`{"name":"v1","kind":"tag","sha":"abc","updated_at":"2026-11-18T12:00:00Z"}`},
	}
	for _, tc := range tests {
		got, err := json.Marshal(tc.v)
		if err != nil || string(got) != tc.want {
			t.Errorf("%s encodes as %s (%v), want %s", tc.name, got, err, tc.want)
		}
	}

	// And they come back out of the database as what went in.
	ctx := context.Background()
	r := open(t)
	c, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Source: SourceMirror,
		Files: []File{file(t, r, "a", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := r.Resolve(ctx, "acme/model", "main"); err != nil || got.Source != SourceMirror || !got.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("resolved %+v, %v; recorded %+v", got, err, c)
	}
	repo, _ := r.GetRepo(ctx, "acme/model")
	refs, err := r.Refs(ctx, repo)
	if err != nil || len(refs) != 2 || refs[0].Kind != KindBranch || refs[1].Kind != KindTag {
		t.Fatalf("refs = %+v, %v", refs, err)
	}
}

// A timestamp the registry cannot read is reported by whatever reads it.
// It used to come back as the year 1 with no error.
func TestUnreadableTimestampIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := parseTime("2026-11-18T12:00:00.000000000Z"); err != nil {
		t.Fatalf("a stored timestamp was refused: %v", err)
	}
	for _, bad := range []string{"", "yesterday", "2026-11-18 12:00:00", "2026-11-18T12:00:00Z"} {
		if got, err := parseTime(bad); err == nil || !got.IsZero() || !strings.Contains(err.Error(), "timestamp") {
			t.Errorf("parseTime(%q) = %v, %v; want an error", bad, got, err)
		}
	}

	ctx := context.Background()
	r := open(t)
	if _, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Files: []File{file(t, r, "a", "a")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateToken(ctx, "ci", RoleRead); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"repos", "commits", "tokens"} {
		if _, err := r.db.ExecContext(ctx, "UPDATE "+table+" SET created_at = 'not a time'"); err != nil { //nolint:gosec // table names are the literals above.
			t.Fatal(err)
		}
	}
	if _, err := r.db.ExecContext(ctx, "UPDATE refs SET updated_at = 'not a time'"); err != nil {
		t.Fatal(err)
	}
	repo := Repo{ID: 1, Name: "acme/model"}
	reads := map[string]func() error{
		"GetRepo":    func() error { _, err := r.GetRepo(ctx, "acme/model"); return err },
		"ListRepos":  func() error { _, err := r.ListRepos(ctx); return err },
		"Commits":    func() error { _, err := r.Commits(ctx, repo); return err },
		"Refs":       func() error { _, err := r.Refs(ctx, repo); return err },
		"Token":      func() error { _, err := r.Token(ctx, "ci"); return err },
		"ListTokens": func() error { _, err := r.ListTokens(ctx); return err },
	}
	for name, read := range reads {
		if err := read(); err == nil || !strings.Contains(err.Error(), `"not a time"`) {
			t.Errorf("%s = %v, want an error naming the unreadable timestamp", name, err)
		}
	}
}
