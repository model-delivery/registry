// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failInserts makes every insert into table fail with a database error
// that has nothing to do with a name being taken.
func failInserts(t *testing.T, r *Registry, table string) {
	t.Helper()
	_, err := r.db.ExecContext(context.Background(),
		"CREATE TRIGGER fail_"+table+" BEFORE INSERT ON "+table+" BEGIN SELECT RAISE(ABORT, 'the disk is full'); END") //nolint:gosec // table names are literals in the tests below.
	if err != nil {
		t.Fatal(err)
	}
}

// Only a name (or secret) that is really taken is "already exists". A
// database failure used to be reported the same way.
func TestSetTokenTellsAConflictFromAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const secret, other = "a-secret-of-sixteen-or-more", "another-secret-of-sixteen"
	r := open(t)
	if err := r.SetToken(ctx, "ci", RoleWrite, secret, false); err != nil {
		t.Fatal(err)
	}

	err := r.SetToken(ctx, "ci", RoleRead, other, false)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), `a token named "ci" already exists`) {
		t.Fatalf("a taken name: %v", err)
	}
	err = r.SetToken(ctx, "deploy", RoleRead, secret, false)
	if !errors.Is(err, ErrConflict) || strings.Contains(err.Error(), `named "deploy" already exists`) {
		t.Fatalf("a secret another token has: %v; want a conflict that does not claim the name is taken", err)
	}
	// Neither refusal changed what is recorded.
	if tok, ok, err := r.Authenticate(ctx, secret); err != nil || !ok || tok.Name != "ci" || tok.Role != RoleWrite {
		t.Fatalf("after the refusals: %+v, %v, %v", tok, ok, err)
	}
	if _, ok, _ := r.Authenticate(ctx, other); ok {
		t.Fatal("a refused token was recorded")
	}
	// Replacing is how a configured admin token is applied on every start.
	if err := r.SetToken(ctx, "ci", RoleAdmin, other, true); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if tok, ok, _ := r.Authenticate(ctx, other); !ok || tok.Role != RoleAdmin {
		t.Fatalf("after replace: %+v, %v", tok, ok)
	}

	failInserts(t, r, "tokens")
	for _, replace := range []bool{false, true} {
		err := r.SetToken(ctx, "new", RoleRead, "yet-another-long-secret", replace)
		if err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the disk is full") {
			t.Errorf("replace=%v with a failing database: %v; want the database's own error", replace, err)
		}
	}
	if _, err := r.CreateToken(ctx, "minted", RoleRead); err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("CreateToken with a failing database: %v", err)
	}
}

func TestCreateBranchTellsAConflictFromAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	c, err := r.Commit(ctx, CommitInput{Repo: "acme/model", Branch: "main", Tags: []string{"v1"}, Files: []File{file(t, r, "a", "a")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, taken := range []string{"main", "v1"} {
		err := r.CreateBranch(ctx, "acme/model", taken, "main")
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("branch %q: %v, want it refused as already existing", taken, err)
		}
	}
	// The tag was not turned into a branch by the refusal.
	repo, _ := r.GetRepo(ctx, "acme/model")
	refs, _ := r.Refs(ctx, repo)
	if len(refs) != 2 || refs[1].Name != "v1" || refs[1].Kind != KindTag || refs[1].SHA != c.SHA {
		t.Fatalf("refs after the refusals = %+v", refs)
	}

	failInserts(t, r, "refs")
	err = r.CreateBranch(ctx, "acme/model", "dev", "main")
	if err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the disk is full") {
		t.Fatalf("with a failing database: %v; want the database's own error, not \"already exists\"", err)
	}
}

// "No token has this secret" and "the database could not be asked" are
// different answers; the second used to look like the first.
func TestAuthenticateTellsUnknownFromUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := open(t)
	secret, err := r.CreateToken(ctx, "ci", RoleWrite)
	if err != nil {
		t.Fatal(err)
	}
	for _, unknown := range []string{"", "md_wrong", secret + "x"} {
		if tok, ok, err := r.Authenticate(ctx, unknown); ok || err != nil || tok != (Token{}) {
			t.Errorf("Authenticate(%q) = %+v, %v, %v; want no token and no error", unknown, tok, ok, err)
		}
	}
	if tok, ok, err := r.Authenticate(ctx, secret); !ok || err != nil || tok.Name != "ci" {
		t.Fatalf("a good token: %+v, %v, %v", tok, ok, err)
	}

	_ = r.Close()
	for _, s := range []string{secret, "md_wrong"} {
		if tok, ok, err := r.Authenticate(ctx, s); err == nil || ok || tok != (Token{}) {
			t.Errorf("with the database closed: %+v, %v, %v; want an error", tok, ok, err)
		}
	}
	// No secret is no lookup, so nothing to fail.
	if _, ok, err := r.Authenticate(ctx, ""); ok || err != nil {
		t.Errorf("no secret, database closed: %v, %v", ok, err)
	}
}
