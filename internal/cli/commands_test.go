// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// These tests run the client commands against a registry that answers
// whatever the test says, which a real one cannot be made to do: a document
// with fields this client has never heard of, a warning, an answer that is
// not a document at all.

// answer is what the scripted registry sends for one route.
type answer struct {
	status int // 0 means 200
	body   string
}

// scripted is a registry that answers each route ("METHOD /path") with a
// fixed answer and remembers what it was asked.
type scripted struct {
	url string

	mu    sync.Mutex
	asked []string
}

func newScripted(t *testing.T, answers map[string]answer) *scripted {
	t.Helper()
	s := &scripted{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.asked = append(s.asked, route)
		s.mu.Unlock()
		a, ok := answers[route]
		if !ok {
			http.Error(w, `{"error":"the test did not expect `+route+`"}`, http.StatusNotImplemented)
			return
		}
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

// run executes modelctl against the scripted registry, with env added to
// its environment.
func (s *scripted) run(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := func(k string) string {
		if v, ok := env[k]; ok {
			return v
		}
		if k == "MODELCTL_ENDPOINT" {
			return s.url
		}
		return ""
	}
	code = Run(context.Background(), args, &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

// requests is the routes the registry was asked for, in order.
func (s *scripted) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// asPrinted is how --json has always printed a document from the registry:
// the document itself, indented, with nothing added, dropped or reordered.
func asPrinted(t *testing.T, document string) string {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(json.RawMessage(document)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Documents as a registry sends them, each with a field no version of this
// client knows and with its fields in an order no Go struct would produce.
const (
	reposDoc = `{"zeta_from_a_newer_registry":{"b":1,"a":2},"repos":[{"updated_at":"2026-11-18T10:00:00.123456789Z",` +
		`"name":"acme/tiny","upstream":"https://huggingface.co","created_at":"2026-11-17T09:00:00Z","revisions":2,"stored_bytes":28183891}]}`
	repoDoc = `{"name":"acme/tiny","created_at":"2026-11-17T09:00:00Z","note":"a <b> & c",` +
		`"refs":[{"name":"main","kind":"branch","sha":"409918bc00000000000000000000000000000000","updated_at":"2026-11-18T10:00:00Z"}],` +
		`"revisions":[{"repo":"acme/tiny","sha":"409918bc00000000000000000000000000000000","source":"push","created_at":"2026-11-18T10:00:00Z","files":3,"bytes":4098}]}`
	freedDoc      = `{"bytes":28183891,"blobs":3}`
	freedWarnDoc  = `{"blobs":1,"bytes":10,"warning":"some unreferenced files could not be removed"}`
	tokenDoc      = `{"name":"ci","role":"write","token":"md_0123","expires":null}`
	tokensDoc     = `{"tokens":[{"name":"admin","role":"admin","created_at":"2026-11-17T09:00:00Z","last_used":"never"}]}`
	backupDoc     = `{"backup":{"name":"registry-20261118T100000Z.db","size":40960,"created_at":"2026-11-18T10:00:00Z"},"contents":{"schema_version":2,"repos":1,"revisions":2,"size":40960}}`
	backupWarnDoc = `{"backup":{"name":"registry-20261118T100000Z.db","size":40960,"created_at":"2026-11-18T10:00:00Z"},"contents":{"schema_version":2,"repos":1,"revisions":2,"size":40960},"warning":"old backups could not be removed"}`
	backupsDoc    = `{"backups":[{"name":"registry-20261118T100000Z.db","size":40960,"created_at":"2026-11-18T10:00:00Z","checksum":"abc"}]}`
)

// --json prints the registry's document as it arrived. That is what it did
// when the commands decoded nothing; it must still, now that they decode
// the document into a type first.
func TestJSONOutputIsTheRegistrysDocument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args     []string
		route    string
		document string
	}{
		{[]string{"list"}, "GET /api/v1/repos", reposDoc},
		{[]string{"list"}, "GET /api/v1/repos", `{"repos":[]}`},
		{[]string{"inspect", "acme/tiny"}, "GET /api/v1/repos/acme/tiny", repoDoc},
		{[]string{"delete", "acme/tiny", "--yes"}, "DELETE /api/v1/repos/acme/tiny", freedDoc},
		{[]string{"delete", "acme/tiny", "--yes"}, "DELETE /api/v1/repos/acme/tiny", freedWarnDoc},
		{[]string{"gc"}, "POST /api/v1/gc", freedDoc},
		{[]string{"gc"}, "POST /api/v1/gc", freedWarnDoc},
		{[]string{"token", "create", "ci", "--role", "write"}, "POST /api/v1/tokens", tokenDoc},
		{[]string{"token", "list"}, "GET /api/v1/tokens", tokensDoc},
		{[]string{"backup", "now"}, "POST /api/v1/backups", backupDoc},
		{[]string{"backup", "now"}, "POST /api/v1/backups", backupWarnDoc},
		{[]string{"backup", "list"}, "GET /api/v1/backups", backupsDoc},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			reg := newScripted(t, map[string]answer{tc.route: {body: tc.document}})
			code, out, errOut := reg.run(t, nil, append(tc.args, "--json")...)
			if code != 0 {
				t.Fatalf("code %d: %s", code, errOut)
			}
			if want := asPrinted(t, tc.document); out != want {
				t.Fatalf("--json printed:\n%s\nwant the registry's document:\n%s", out, want)
			}
			// With --json, standard output is the document and nothing is
			// said beside it.
			if errOut != "" {
				t.Fatalf("stderr with --json: %q", errOut)
			}
		})
	}

	// One of them spelled out, so that the comparison above is not the only
	// thing holding the format: two spaces, the registry's field order, a
	// field this client does not know, and a final newline.
	t.Run("byte for byte", func(t *testing.T) {
		t.Parallel()
		reg := newScripted(t, map[string]answer{"POST /api/v1/tokens": {status: http.StatusCreated, body: tokenDoc}})
		_, out, _ := reg.run(t, nil, "token", "create", "ci", "--role", "write", "--json")
		const want = "{\n  \"name\": \"ci\",\n  \"role\": \"write\",\n  \"token\": \"md_0123\",\n  \"expires\": null\n}\n"
		if out != want {
			t.Fatalf("printed %q, want %q", out, want)
		}
	})

	t.Run("inspect --revision wraps the two documents as before", func(t *testing.T) {
		t.Parallel()
		reg := newScripted(t, map[string]answer{
			"GET /api/v1/repos/acme/tiny":                {body: repoDoc},
			"GET /api/v1/repos/acme/tiny/revisions/main": {body: `{"repo":"acme/tiny","sha":"409918bc00000000000000000000000000000000","files":[{"path":"config.json","size":2,"sha256":"aa","git_oid":"bb","lfs":true}]}`},
		})
		code, out, errOut := reg.run(t, nil, "inspect", "acme/tiny", "--revision", "main", "--json")
		if code != 0 {
			t.Fatalf("code %d: %s", code, errOut)
		}
		var doc struct {
			Repository json.RawMessage `json:"repository"`
			Revision   struct {
				Repo  string `json:"repo"`
				SHA   string `json:"sha"`
				Files []struct {
					Path    string `json:"path"`
					Pending *bool  `json:"pending"`
				} `json:"files"`
			} `json:"revision"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		var got, want bytes.Buffer
		if err := json.Compact(&got, doc.Repository); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&want, []byte(repoDoc)); err != nil {
			t.Fatal(err)
		}
		// The encoder writes <, > and & as escapes; the document is the same.
		if strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(got.String()) != want.String() {
			t.Fatalf("repository = %s, want the registry's document", got.String())
		}
		if doc.Revision.SHA == "" || len(doc.Revision.Files) != 1 || doc.Revision.Files[0].Pending == nil {
			t.Fatalf("revision = %+v", doc.Revision)
		}
	})
}

// What the commands print for people is unchanged by where the decoding
// now happens.
func TestHumanOutputOfTheMovedCommands(t *testing.T) {
	t.Parallel()
	reg := newScripted(t, map[string]answer{
		"GET /api/v1/repos":              {body: reposDoc},
		"GET /api/v1/repos/acme/tiny":    {body: repoDoc},
		"DELETE /api/v1/repos/acme/tiny": {body: freedDoc},
		"POST /api/v1/gc":                {body: freedDoc},
		"POST /api/v1/tokens":            {status: http.StatusCreated, body: tokenDoc},
		"GET /api/v1/tokens":             {body: tokensDoc},
		"DELETE /api/v1/tokens/ci":       {status: http.StatusNoContent},
		"POST /api/v1/backups":           {status: http.StatusCreated, body: backupDoc},
		"GET /api/v1/backups":            {body: backupsDoc},
	})
	tests := []struct {
		args       []string
		wantStdout []string
		wantStderr string
	}{
		{[]string{"list"}, []string{"REPOSITORY", "SOURCE", "acme/tiny", "mirror", "26.9 MiB"}, ""},
		{[]string{"inspect", "acme/tiny"}, []string{"Repository: acme/tiny\n", "main", "branch", "409918bc00000000000000000000000000000000", "push", "4.0 KiB"}, ""},
		{[]string{"delete", "acme/tiny", "--yes"}, []string{"Deleted acme/tiny: freed 26.9 MiB in 3 stored files\n"}, ""},
		{[]string{"gc"}, []string{"Removed 3 unreferenced stored files, freed 26.9 MiB\n"}, ""},
		{[]string{"token", "create", "ci", "--role", "write"}, []string{"md_0123\n"}, "Created write token \"ci\". It is shown only this once:\n"},
		{[]string{"token", "list"}, []string{"NAME", "ROLE", "CREATED", "admin"}, ""},
		{[]string{"token", "revoke", "ci"}, []string{"Revoked token \"ci\"\n"}, ""},
		{[]string{"backup", "now"}, []string{"Stored backup registry-20261118T100000Z.db (40.0 KiB): 1 repositories, 2 revisions\n"}, ""},
		{[]string{"backup", "list"}, []string{"BACKUP", "SIZE", "CREATED", "registry-20261118T100000Z.db", "40.0 KiB"}, ""},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			code, out, errOut := reg.run(t, nil, tc.args...)
			if code != 0 {
				t.Fatalf("code %d: %s", code, errOut)
			}
			for _, want := range tc.wantStdout {
				if !strings.Contains(out, want) {
					t.Errorf("stdout lacks %q:\n%s", want, out)
				}
			}
			if errOut != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", errOut, tc.wantStderr)
			}
		})
	}
}

// The registry answers with a warning when it did what was asked and could
// not then finish tidying up. The command succeeds and passes the warning
// on, on standard error, where a script reading the output is not confused
// by it.
func TestWarningsFromTheRegistryAreShown(t *testing.T) {
	t.Parallel()
	const freedWarning = "Warning: some unreferenced files could not be removed\n"
	tests := []struct {
		args       []string
		route      string
		document   string
		wantStdout string
		wantStderr string
	}{
		{[]string{"delete", "acme/tiny", "--yes"}, "DELETE /api/v1/repos/acme/tiny", freedWarnDoc, "Deleted acme/tiny: freed 10 B in 1 stored files\n", freedWarning},
		{[]string{"gc"}, "POST /api/v1/gc", freedWarnDoc, "Removed 1 unreferenced stored files, freed 10 B\n", freedWarning},
		{[]string{"backup", "now"}, "POST /api/v1/backups", backupWarnDoc,
			"Stored backup registry-20261118T100000Z.db (40.0 KiB): 1 repositories, 2 revisions\n", "Warning: old backups could not be removed\n"},
		// And nothing is said when there is nothing to say.
		{[]string{"delete", "acme/tiny", "--yes"}, "DELETE /api/v1/repos/acme/tiny", freedDoc, "Deleted acme/tiny: freed 26.9 MiB in 3 stored files\n", ""},
		{[]string{"gc"}, "POST /api/v1/gc", freedDoc, "Removed 3 unreferenced stored files, freed 26.9 MiB\n", ""},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Parallel()
			reg := newScripted(t, map[string]answer{tc.route: {body: tc.document}})
			code, out, errOut := reg.run(t, nil, tc.args...)
			if code != 0 || out != tc.wantStdout || errOut != tc.wantStderr {
				t.Fatalf("code %d\nstdout %q, want %q\nstderr %q, want %q", code, out, tc.wantStdout, errOut, tc.wantStderr)
			}
		})
	}
}

// An answer that cannot be read is an error. It used to be ignored by six
// commands: `token create` printed an empty line as the token and exited 0,
// and `delete` and `gc` reported that nothing was freed.
func TestAnAnswerThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()
	commands := []struct {
		args  []string
		route string
	}{
		{[]string{"list"}, "GET /api/v1/repos"},
		{[]string{"inspect", "acme/tiny"}, "GET /api/v1/repos/acme/tiny"},
		{[]string{"delete", "acme/tiny", "--yes"}, "DELETE /api/v1/repos/acme/tiny"},
		{[]string{"gc"}, "POST /api/v1/gc"},
		{[]string{"token", "create", "ci"}, "POST /api/v1/tokens"},
		{[]string{"token", "list"}, "GET /api/v1/tokens"},
		{[]string{"backup", "now"}, "POST /api/v1/backups"},
		{[]string{"backup", "list"}, "GET /api/v1/backups"},
	}
	answers := []struct {
		name, body, wantErr string
	}{
		{"a page that is not JSON", "<html>502 from a proxy that answers 200</html>", "unreadable answer from the registry"},
		{"nothing at all", "", "unreadable answer from the registry"},
		{"JSON of another shape", `["not", "an", "object"]`, "unexpected answer from the registry"},
		{"a field of the wrong type", `{"repos":"many","blobs":"three","tokens":7,"backups":{},"backup":[],"name":1,"token":[]}`, "unexpected answer from the registry"},
	}
	for _, c := range commands {
		for _, a := range answers {
			for _, jsonFlag := range []bool{false, true} {
				args := c.args
				if jsonFlag {
					args = append(append([]string{}, args...), "--json")
				}
				t.Run(strings.Join(args, " ")+": "+a.name, func(t *testing.T) {
					t.Parallel()
					reg := newScripted(t, map[string]answer{c.route: {body: a.body}})
					code, out, errOut := reg.run(t, nil, args...)
					if code != 1 || !strings.Contains(errOut, a.wantErr) {
						t.Fatalf("code %d, stderr %q; want exit 1 and %q", code, errOut, a.wantErr)
					}
					if out != "" {
						t.Fatalf("printed %q although the answer could not be read", out)
					}
				})
			}
		}
	}

	t.Run("token create: a document without the secret", func(t *testing.T) {
		t.Parallel()
		reg := newScripted(t, map[string]answer{"POST /api/v1/tokens": {body: `{"name":"ci","role":"read"}`}})
		code, out, errOut := reg.run(t, nil, "token", "create", "ci")
		if code != 1 || out != "" || !strings.Contains(errOut, "without the new token's secret") {
			t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
		}
		if strings.Contains(errOut, "Created") {
			t.Fatalf("announced a token it did not get: %q", errOut)
		}
	})
}

// Revoking a token is answered 204 with no body. --json used to print
// nothing for it; every other command prints a document.
func TestTokenRevokeJSON(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	if code, _, errOut := f.run(t, f.admin, "token", "create", "ci", "--role", "write"); code != 0 {
		t.Fatalf("token create: %s", errOut)
	}
	code, out, errOut := f.run(t, f.admin, "token", "revoke", "ci", "--json")
	const want = "{\n  \"name\": \"ci\",\n  \"revoked\": true\n}\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("code %d, stdout %q (want %q), stderr %q", code, out, want, errOut)
	}
	// It did revoke it: a second time there is no such token.
	code, out, errOut = f.run(t, f.admin, "token", "revoke", "ci", "--json")
	if code != 1 || out != "" || !strings.Contains(errOut, "token not found") {
		t.Fatalf("revoking it again: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	// Without --json the line for people is what it was.
	if code, _, errOut := f.run(t, f.admin, "token", "create", "ci2"); code != 0 {
		t.Fatalf("token create: %s", errOut)
	}
	if code, out, _ := f.run(t, f.admin, "token", "revoke", "ci2"); code != 0 || out != "Revoked token \"ci2\"\n" {
		t.Fatalf("revoke: code %d, stdout %q", code, out)
	}
}

// The snapshot is downloaded from the address beside the other backup
// routes. The one v0.3.0 used is still served, for clients of that version.
func TestBackupDownloadUsesTheBackupsAddress(t *testing.T) {
	t.Parallel()
	reg := newScripted(t, map[string]answer{"GET /api/v1/backups/snapshot": {body: "not a database"}})
	dst := filepath.Join(t.TempDir(), "snapshot.db")
	code, _, errOut := reg.run(t, nil, "backup", "download", dst)
	// What arrived is not a snapshot, so the command refuses it and removes it.
	if code != 1 || !strings.Contains(errOut, "failed its check and was removed") {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("the file that failed its check was left: %v", err)
	}
	if asked := reg.requests(); len(asked) != 1 || asked[0] != "GET /api/v1/backups/snapshot" {
		t.Fatalf("asked for %v", asked)
	}

	// A file already there is never overwritten, and is not removed either.
	if err := os.WriteFile(dst, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := reg.run(t, nil, "backup", "download", dst); code == 0 {
		t.Fatal("downloaded over an existing file")
	}
	if got, _ := os.ReadFile(dst); string(got) != "mine" {
		t.Fatalf("the existing file now holds %q", got)
	}
}

// --endpoint, --token and --upstream-token are each resolved the same way:
// the flag, else the environment, else the default.
func TestClientOptionsComeFromFlagThenEnvironment(t *testing.T) {
	t.Parallel()

	t.Run("the rule", func(t *testing.T) {
		t.Parallel()
		vars := map[string]string{"FIRST": "first", "SECOND": "second"}
		e := &env{getenv: func(k string) string { return vars[k] }}
		tests := []struct {
			flag, fallback string
			keys           []string
			want           string
		}{
			{"flag", "default", []string{"FIRST", "SECOND"}, "flag"},
			{"", "default", []string{"FIRST", "SECOND"}, "first"},
			{"", "default", []string{"UNSET", "SECOND"}, "second"},
			{"", "default", []string{"UNSET"}, "default"},
			{"", "", nil, ""},
		}
		for _, tc := range tests {
			if got := e.option(tc.flag, tc.fallback, tc.keys...); got != tc.want {
				t.Errorf("option(%q, %q, %v) = %q, want %q", tc.flag, tc.fallback, tc.keys, got, tc.want)
			}
		}
	})

	const done = `{"event":"done","repo":"acme/tiny","sha":"409918bc00000000000000000000000000000000"}` + "\n"
	tests := []struct {
		name      string
		env       map[string]string
		flags     []string
		wantToken string // the registry token the request carried
		wantUp    string // the upstream token it passed on
	}{
		{"nothing set", nil, nil, "", ""},
		{"from the environment", map[string]string{"MODELCTL_TOKEN": "env-token", "MODELCTL_UPSTREAM_TOKEN": "env-up"}, nil, "env-token", "env-up"},
		{"HF_TOKEN stands in for the upstream token", map[string]string{"HF_TOKEN": "hf-token"}, nil, "", "hf-token"},
		{"MODELCTL_UPSTREAM_TOKEN comes before HF_TOKEN", map[string]string{"MODELCTL_UPSTREAM_TOKEN": "env-up", "HF_TOKEN": "hf-token"}, nil, "", "env-up"},
		{"flags come before the environment", map[string]string{"MODELCTL_TOKEN": "env-token", "MODELCTL_UPSTREAM_TOKEN": "env-up", "HF_TOKEN": "hf-token"},
			[]string{"--token", "flag-token", "--upstream-token", "flag-up"}, "flag-token", "flag-up"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var auth string
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				auth = r.Header.Get("Authorization")
				mu.Unlock()
				sent, _ := io.ReadAll(r.Body)
				var req struct {
					UpstreamToken string `json:"upstream_token"`
				}
				_ = json.Unmarshal(sent, &req)
				w.Header().Set("X-Upstream-Token", req.UpstreamToken)
				_, _ = io.WriteString(w, strings.Replace(done, `"repo"`, `"ref":"`+req.UpstreamToken+`","repo"`, 1))
			}))
			t.Cleanup(srv.Close)

			// The environment names a registry nothing listens at; --endpoint
			// is what reaches the test's one.
			env := map[string]string{"MODELCTL_ENDPOINT": "http://127.0.0.1:1"}
			for k, v := range tc.env {
				env[k] = v
			}
			args := append([]string{"mirror", "acme/tiny", "--json", "--endpoint", srv.URL}, tc.flags...)
			var out, errOut bytes.Buffer
			if code := Run(context.Background(), args, &out, &errOut, func(k string) string { return env[k] }); code != 0 {
				t.Fatalf("code %d: %s", code, errOut.String())
			}
			var printed struct {
				Ref string `json:"ref"`
			}
			if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
				t.Fatal(err)
			}
			wantAuth := ""
			if tc.wantToken != "" {
				wantAuth = "Bearer " + tc.wantToken
			}
			mu.Lock()
			defer mu.Unlock()
			if auth != wantAuth || printed.Ref != tc.wantUp {
				t.Fatalf("registry token %q (want %q), upstream token %q (want %q)", auth, wantAuth, printed.Ref, tc.wantUp)
			}
		})
	}

	t.Run("the endpoint: environment, then the default", func(t *testing.T) {
		t.Parallel()
		reg := newScripted(t, map[string]answer{"GET /api/v1/repos": {body: `{"repos":[]}`}})
		// run supplies MODELCTL_ENDPOINT, and no flag is given.
		if code, out, errOut := reg.run(t, nil, "list"); code != 0 || !strings.Contains(out, "No repositories yet") {
			t.Fatalf("with MODELCTL_ENDPOINT: code %d %q %q", code, out, errOut)
		}
		// Neither: the default. Looked at without sending anything there.
		c := newClientCommand(&env{stderr: io.Discard, getenv: func(string) string { return "" }}, "list", "list")
		if _, err := c.parse(nil, 0, 0); err != nil {
			t.Fatal(err)
		}
		if got := c.client(); got.Endpoint != "http://localhost:8080" || got.Token != "" {
			t.Fatalf("with nothing set: endpoint %q, token %q", got.Endpoint, got.Token)
		}
	})

	t.Run("--help prints no token from the environment", func(t *testing.T) {
		t.Parallel()
		env := map[string]string{"MODELCTL_TOKEN": "md_secret_from_the_environment", "MODELCTL_UPSTREAM_TOKEN": "hf_secret_from_the_environment", "MODELCTL_ENDPOINT": "http://registry.internal:8080"}
		var out, errOut bytes.Buffer
		code := Run(context.Background(), []string{"mirror", "--help"}, &out, &errOut, func(k string) string { return env[k] })
		help := out.String() + errOut.String()
		if code != 0 || strings.Contains(help, "secret_from_the_environment") {
			t.Fatalf("code %d, help:\n%s", code, help)
		}
		for _, want := range []string{"MODELCTL_ENDPOINT", "http://localhost:8080", "MODELCTL_TOKEN", "MODELCTL_UPSTREAM_TOKEN", "HF_TOKEN"} {
			if !strings.Contains(help, want) {
				t.Errorf("help does not mention %s:\n%s", want, help)
			}
		}
	})
}
