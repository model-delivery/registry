// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package client is the Go client for a registry server. modelctl is a
// thin shell around it: every request the command line makes is a method
// here, and every answer is a named type.
//
// The package shares only the HTTP API with the server (docs/api.md): it
// declares the documents it reads itself and imports none of the server's
// types.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/kit"
)

// Client talks to one registry.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// New returns a client for the registry at endpoint.
func New(endpoint, token string) *Client {
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), Token: token, HTTP: &http.Client{}}
}

// APIError is a non-2xx answer from the registry.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "not authorized: set MODELCTL_TOKEN (or --token) to a valid access token"
	case http.StatusForbidden:
		return "forbidden: " + e.Message
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

const (
	// maxErrorBody is how much of an error answer is read for its message.
	maxErrorBody = 4096
	// Progress streams (mirror, verify) are read a line at a time: a buffer
	// of progressLineBuffer to start with, growing to maxProgressLine for a
	// long line. Nothing the registry sends comes near the larger figure.
	progressLineBuffer = 64 << 10
	maxProgressLine    = 4 << 20
)

// request is one call to the registry. Only method and path are needed;
// the rest is for requests that send something.
type request struct {
	method string
	// path is the escaped path and query, starting with a slash.
	path   string
	header http.Header
	body   io.Reader
	// size is the length of body when net/http cannot tell it for itself,
	// as with a file. Left at zero, the body is sent without a declared
	// length unless net/http knows it.
	size int64
}

// get is the request for reading path.
func get(path string) request { return request{method: http.MethodGet, path: path} }

// do sends req and returns the answer if it is a success. Anything else
// comes back as an *APIError carrying the registry's message.
func (c *Client) do(ctx context.Context, r request) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, r.method, c.Endpoint+r.path, r.body)
	if err != nil {
		return nil, err
	}
	if r.body != nil && r.size > 0 {
		req.ContentLength = r.size
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the registry at %s: %w", c.Endpoint, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var doc struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &doc) == nil && doc.Error != "" {
		msg = doc.Error
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, &APIError{Status: resp.StatusCode, Message: msg}
}

// jsonRequest is a request that sends in as its JSON body; nil sends none.
func jsonRequest(method, path string, in any) (request, error) {
	r := request{method: method, path: path}
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return request{}, err
		}
		r.body, r.header = bytes.NewReader(raw), http.Header{"Content-Type": {"application/json"}}
	}
	return r, nil
}

// document performs a JSON request and decodes the answer into out. It
// also returns the answer exactly as the registry sent it, which is what
// the result types keep in their Raw field. An answer that is not the
// document expected is an error, never a zero value.
func (c *Client) document(ctx context.Context, method, path string, in, out any) (json.RawMessage, error) {
	req, err := jsonRequest(method, path, in)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("unreadable answer from the registry at %s: %w", c.Endpoint, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("unexpected answer from the registry at %s: %w", c.Endpoint, err)
	}
	return raw, nil
}

// RepoType is a repository's type, as the registry's API names it.
type RepoType string

// The repository types. A document that names no type describes a model.
const (
	Model   RepoType = "model"
	Dataset RepoType = "dataset"
)

// ParseRepoType reads a repository type as a person writes one (the hf
// CLI's --repo-type). The empty string is a model.
func ParseRepoType(s string) (RepoType, error) {
	switch RepoType(s) {
	case "", Model:
		return Model, nil
	case Dataset:
		return Dataset, nil
	}
	return "", fmt.Errorf("repository type must be model or dataset, not %q", s)
}

// RepoKey is how this client's methods name a repository: by its name for
// a model, and "datasets/" and its name for a dataset, which is the path
// the Hub gives each.
func RepoKey(typ RepoType, name string) string {
	if typ == Dataset {
		return kit.DatasetKey(name)
	}
	return name
}

// repoPath is the API path of the repository with key repo: under
// /api/v1/repos for a model and /api/v1/datasets for a dataset.
func repoPath(repo string) string {
	if name, dataset := kit.SplitKey(repo); dataset {
		return "/api/v1/datasets/" + kit.EscapePath(name)
	}
	return "/api/v1/repos/" + kit.EscapePath(repo)
}

// ---- repositories ----

// The result types below each carry Raw: the document as the registry sent
// it, including any field this client does not know. modelctl prints it
// for --json, so that output does not depend on the client's version.

// RepoSummary is one repository in a listing.
type RepoSummary struct {
	Name string `json:"name"`
	// Type is Dataset for a dataset and empty for a model.
	Type RepoType `json:"type,omitempty"`
	// Upstream is the hub the repository was mirrored from; empty for one
	// created by a push or an upload.
	Upstream    string    `json:"upstream"`
	CreatedAt   time.Time `json:"created_at"`
	Revisions   int       `json:"revisions"`
	StoredBytes int64     `json:"stored_bytes"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RepoList is the registry's repositories, by name.
type RepoList struct {
	Repos []RepoSummary   `json:"repos"`
	Raw   json.RawMessage `json:"-"`
}

// ListRepos lists every model.
func (c *Client) ListRepos(ctx context.Context) (RepoList, error) {
	return c.ListRepositories(ctx, Model)
}

// ListRepositories lists every repository of type typ.
func (c *Client) ListRepositories(ctx context.Context, typ RepoType) (RepoList, error) {
	path := "/api/v1/repos"
	if typ == Dataset {
		path = "/api/v1/datasets"
	}
	var out RepoList
	raw, err := c.document(ctx, http.MethodGet, path, nil, &out)
	out.Raw = raw
	return out, err
}

// Ref is a branch or tag and the revision it names.
type Ref struct {
	Name string `json:"name"`
	// Kind is "branch" or "tag".
	Kind string `json:"kind"`
	SHA  string `json:"sha"`
}

// RevisionSummary is one revision in a repository's history.
type RevisionSummary struct {
	SHA string `json:"sha"`
	// Source is "push" or "mirror".
	Source string `json:"source"`
	// Files is how many files the revision has, and Bytes their total size.
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// RepoDetail is a repository with its refs and its revisions, newest first.
type RepoDetail struct {
	Name      string            `json:"name"`
	Type      RepoType          `json:"type,omitempty"` // Dataset, or empty for a model
	Upstream  string            `json:"upstream"`
	Refs      []Ref             `json:"refs"`
	Revisions []RevisionSummary `json:"revisions"`
	Raw       json.RawMessage   `json:"-"`
}

// Repo describes one repository; repo is its key (RepoKey).
func (c *Client) Repo(ctx context.Context, repo string) (RepoDetail, error) {
	var out RepoDetail
	raw, err := c.document(ctx, http.MethodGet, repoPath(repo), nil, &out)
	out.Raw = raw
	return out, err
}

// Freed is the answer to an operation that removes stored files: how many
// were freed and how much space that was.
type Freed struct {
	Blobs int   `json:"blobs"`
	Bytes int64 `json:"bytes"`
	// Warning is set when the operation was done and some files could not
	// then be removed from storage. It is not an error: what was asked for
	// happened.
	Warning string          `json:"warning"`
	Raw     json.RawMessage `json:"-"`
}

// DeleteRepo deletes the repository with key repo and frees the files only
// it used.
func (c *Client) DeleteRepo(ctx context.Context, repo string) (Freed, error) {
	var out Freed
	raw, err := c.document(ctx, http.MethodDelete, repoPath(repo), nil, &out)
	out.Raw = raw
	return out, err
}

// GC removes stored files that no revision references.
func (c *Client) GC(ctx context.Context) (Freed, error) {
	var out Freed
	raw, err := c.document(ctx, http.MethodPost, "/api/v1/gc", nil, &out)
	out.Raw = raw
	return out, err
}

// ---- tokens ----

// CreatedToken is a new access token. Token is its secret, which the
// registry shows this once and does not keep.
type CreatedToken struct {
	Name  string          `json:"name"`
	Role  string          `json:"role"`
	Token string          `json:"token"`
	Raw   json.RawMessage `json:"-"`
}

// CreateToken mints an access token with the given role: "read", "write"
// or "admin".
func (c *Client) CreateToken(ctx context.Context, name, role string) (CreatedToken, error) {
	var out CreatedToken
	raw, err := c.document(ctx, http.MethodPost, "/api/v1/tokens", map[string]string{"name": name, "role": role}, &out)
	if err == nil && out.Token == "" {
		err = fmt.Errorf("the registry at %s answered without the new token's secret", c.Endpoint)
	}
	out.Raw = raw
	return out, err
}

// TokenInfo is an access token as listed: never its secret.
type TokenInfo struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// TokenList is the registry's access tokens.
type TokenList struct {
	Tokens []TokenInfo     `json:"tokens"`
	Raw    json.RawMessage `json:"-"`
}

// ListTokens lists the access tokens.
func (c *Client) ListTokens(ctx context.Context) (TokenList, error) {
	var out TokenList
	raw, err := c.document(ctx, http.MethodGet, "/api/v1/tokens", nil, &out)
	out.Raw = raw
	return out, err
}

// RevokeToken makes the token with this name stop working. The registry
// answers a revocation with no document.
func (c *Client) RevokeToken(ctx context.Context, name string) error {
	resp, err := c.do(ctx, request{method: http.MethodDelete, path: "/api/v1/tokens/" + url.PathEscape(name)})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ---- backups ----

// Backup is one stored metadata backup.
type Backup struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// SnapshotContents is what a verified snapshot of the metadata holds.
type SnapshotContents struct {
	SchemaVersion int   `json:"schema_version"`
	Repos         int   `json:"repos"`
	Revisions     int   `json:"revisions"`
	Size          int64 `json:"size"`
}

// BackupResult is a backup that was just taken and stored.
type BackupResult struct {
	Backup   Backup           `json:"backup"`
	Contents SnapshotContents `json:"contents"`
	// Warning is set when the backup was stored and something after that
	// went wrong, such as removing old backups.
	Warning string          `json:"warning"`
	Raw     json.RawMessage `json:"-"`
}

// BackupNow takes a backup of the metadata and stores it with the model
// files.
func (c *Client) BackupNow(ctx context.Context) (BackupResult, error) {
	var out BackupResult
	raw, err := c.document(ctx, http.MethodPost, "/api/v1/backups", nil, &out)
	out.Raw = raw
	return out, err
}

// BackupList is the stored backups, oldest first.
type BackupList struct {
	Backups []Backup        `json:"backups"`
	Raw     json.RawMessage `json:"-"`
}

// ListBackups lists the stored backups.
func (c *Client) ListBackups(ctx context.Context) (BackupList, error) {
	var out BackupList
	raw, err := c.document(ctx, http.MethodGet, "/api/v1/backups", nil, &out)
	out.Raw = raw
	return out, err
}

// DownloadSnapshot saves a fresh snapshot of the metadata database to dst,
// which must not exist, and returns how many bytes were written. A
// download that fails part-way leaves no file.
func (c *Client) DownloadSnapshot(ctx context.Context, dst string) (int64, error) {
	resp, err := c.do(ctx, get("/api/v1/backups/snapshot"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return kit.WriteNewFile(dst, resp.Body)
}

// ---- revisions ----

// File is one file of a revision as the registry reports it.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// GitOID is the file's git object id. SHA256 is empty for a file the
	// registry lists but has not fetched from its upstream yet, unless the
	// file is LFS-tracked; such a file is then verified against GitOID,
	// which for a non-LFS file is the git blob id of its content.
	GitOID string `json:"git_oid"`
	LFS    bool   `json:"lfs"`
	// Pending is true while the registry has not fetched the file's content.
	Pending bool `json:"pending"`
}

// matches reports whether the file at path has the content f describes.
func (f File) matches(path string) bool {
	if f.SHA256 != "" {
		digest, _, err := hashFile(path)
		return err == nil && digest == f.SHA256
	}
	oid, err := gitBlobID(path)
	return err == nil && !f.LFS && oid == f.GitOID
}

// gitBlobID is the id git gives the content of the file at path.
func gitBlobID(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // a path under the directory the user named.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	fi, err := file.Stat()
	if err != nil {
		return "", err
	}
	return kit.GitBlobID(file, fi.Size())
}

// Revision is a revision and its files.
type Revision struct {
	Repo     string   `json:"repo"`
	RepoType RepoType `json:"repo_type,omitempty"` // Dataset, or empty for a model
	SHA      string   `json:"sha"`
	Files    []File   `json:"files"`
}

// Key is the key (RepoKey) of the revision's repository.
func (r Revision) Key() string { return RepoKey(r.RepoType, r.Repo) }

// Revision resolves a branch, tag or revision id of the repository with key
// repo.
func (c *Client) Revision(ctx context.Context, repo, rev string) (Revision, error) {
	var out Revision
	_, err := c.document(ctx, http.MethodGet, repoPath(repo)+"/revisions/"+url.PathEscape(rev), nil, &out)
	return out, err
}

// ---- progress streams ----

// The kinds of line in a progress stream. Mirror sends EventStart, an
// EventFile per file, then EventDone; Verify sends EventStart, an
// EventBlob per stored file, then EventDone. Either ends with EventError
// instead when it could not finish.
const (
	EventStart = "start"
	EventFile  = "file"
	EventBlob  = "blob"
	EventDone  = "done"
	EventError = "error"
)

// VerifyOK is the Status of a stored file that passed the check.
const VerifyOK = "ok"

// progressLine is a line of a progress stream: it says which kind it is
// and, for an EventError, what went wrong.
type progressLine interface {
	outcome() (kind, failure string)
}

// cutShort is what a caller is told when a progress stream stops before
// its last line: lost when the connection failed, ended when the registry
// closed it.
type cutShort struct{ lost, ended string }

// stream sends req and reads the answer as a stream of JSON lines. Each
// line goes to progress, until the EventDone line, which is the result, or
// an EventError line, which is the error. A stream that ends before either
// is an error too: without its last line nothing says how the run went.
func stream[E progressLine](ctx context.Context, c *Client, req request, cut cutShort, progress func(E)) (E, error) {
	var none E
	resp, err := c.do(ctx, req)
	if err != nil {
		return none, err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, progressLineBuffer), maxProgressLine)
	for sc.Scan() {
		var e E
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return none, fmt.Errorf("unreadable progress line from registry: %w", err)
		}
		switch kind, failure := e.outcome(); kind {
		case EventError:
			return none, errors.New(failure)
		case EventDone:
			return e, nil
		}
		if progress != nil {
			progress(e)
		}
	}
	if err := sc.Err(); err != nil {
		return none, fmt.Errorf("%s: %w", cut.lost, err)
	}
	return none, errors.New(cut.ended)
}

// MirrorEvent is one progress line from a mirror run.
type MirrorEvent struct {
	Event           string   `json:"event"`
	Repo            string   `json:"repo"`
	RepoType        RepoType `json:"repo_type,omitempty"` // Dataset, or empty for a model
	SHA             string   `json:"sha"`
	Ref             string   `json:"ref"`
	Path            string   `json:"path"`
	Status          string   `json:"status"`
	Size            int64    `json:"size"`
	Files           int      `json:"files"`
	Bytes           int64    `json:"bytes"`
	DownloadedBytes int64    `json:"downloaded_bytes"`
	ReusedBytes     int64    `json:"reused_bytes"`
	Error           string   `json:"error"`
}

// Mirror asks the registry to import repo@revision from its upstream and
// reports progress as it happens. repo is a key (RepoKey), so a dataset is
// "datasets/namespace/name". It returns the final "done" event.
func (c *Client) Mirror(ctx context.Context, repo, revision, upstreamToken string, progress func(MirrorEvent)) (MirrorEvent, error) {
	body := map[string]string{"repo": repo, "revision": revision, "upstream_token": upstreamToken}
	if name, dataset := kit.SplitKey(repo); dataset {
		// Only a dataset names its type, so a model's request is what it
		// always was; a registry too old for datasets refuses the field.
		body["repo"], body["type"] = name, string(Dataset)
	}
	req, err := jsonRequest(http.MethodPost, "/api/v1/mirror", body)
	if err != nil {
		return MirrorEvent{}, err
	}
	return stream(ctx, c, req, cutShort{
		lost:  "connection to registry lost during mirror",
		ended: "registry closed the connection before the mirror finished",
	}, progress)
}

func (e MirrorEvent) outcome() (kind, failure string) { return e.Event, e.Error }

// VerifyEvent is one line of a verify run: "start", then a "blob" per
// stored file, then "done".
type VerifyEvent struct {
	Event string `json:"event"`
	// For "blob": the file checked and what was found. Status is "ok",
	// "missing", "damaged" or "unreadable"; Repos names repositories that
	// use a file something is wrong with.
	SHA256 string   `json:"sha256,omitempty"`
	Size   int64    `json:"size,omitempty"`
	Status string   `json:"status,omitempty"`
	Detail string   `json:"detail,omitempty"`
	Repos  []string `json:"repos,omitempty"`
	// For "start": what is about to be checked. For "done": what was.
	Blobs  int    `json:"blobs"`
	Bytes  int64  `json:"bytes"`
	Failed int    `json:"failed"`
	Error  string `json:"error,omitempty"`
}

// Verify asks the registry to re-read every stored file and check it
// against its digest, reporting each file as it is checked. It returns the
// final "done" event. Files that fail the check are reported through
// progress and counted in the result; they are not an error.
func (c *Client) Verify(ctx context.Context, progress func(VerifyEvent)) (VerifyEvent, error) {
	return stream(ctx, c, request{method: http.MethodPost, path: "/api/v1/verify"}, cutShort{
		lost:  "connection to registry lost during the check",
		ended: "registry closed the connection before the check finished; run it again",
	}, progress)
}

func (e VerifyEvent) outcome() (kind, failure string) { return e.Event, e.Error }

// ---- push ----

// PushOptions controls Push.
type PushOptions struct {
	Branch  string
	Tags    []string
	Message string
	Merge   bool
	// Progress is called once per file with whether it had to be uploaded.
	Progress func(path string, size int64, uploaded bool)
}

// PushResult is the revision a push produced.
type PushResult struct {
	Repo     string   `json:"repo"`
	RepoType RepoType `json:"repo_type,omitempty"` // Dataset, or empty for a model
	SHA      string   `json:"sha"`
}

// Push uploads the files under dir and records them as a new revision of
// the repository with key repo. Files the registry already holds (by
// SHA-256) are not sent again.
func (c *Client) Push(ctx context.Context, dir, repo string, opt PushOptions) (PushResult, error) {
	type entry struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	var files []entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (name == ".git" || name == ".cache") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		digest, size, err := hashFile(p)
		if err != nil {
			return err
		}
		uploaded, err := c.uploadBlob(ctx, p, digest, size)
		if err != nil {
			return fmt.Errorf("upload %s: %w", rel, err)
		}
		if opt.Progress != nil {
			opt.Progress(rel, size, uploaded)
		}
		files = append(files, entry{Path: rel, SHA256: digest})
		return nil
	})
	if err != nil {
		return PushResult{}, err
	}
	if len(files) == 0 {
		return PushResult{}, fmt.Errorf("no files found under %s", dir)
	}
	var out PushResult
	_, err = c.document(ctx, http.MethodPost, repoPath(repo)+"/commits", map[string]any{
		"branch": opt.Branch, "tags": opt.Tags, "message": opt.Message, "merge": opt.Merge, "files": files,
	}, &out)
	return out, err
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p) //nolint:gosec // path comes from walking the directory the user named.
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// uploadBlob sends the file at p unless the registry already stores its
// content, and reports whether it had to.
func (c *Client) uploadBlob(ctx context.Context, p, digest string, size int64) (bool, error) {
	blobPath := "/api/v1/blobs/" + digest
	resp, err := c.do(ctx, request{method: http.MethodHead, path: blobPath})
	if err == nil {
		_ = resp.Body.Close()
		return false, nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return false, err
	}
	f, err := os.Open(p) //nolint:gosec // see hashFile.
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	resp, err = c.do(ctx, request{
		method: http.MethodPut, path: blobPath, body: f, size: size,
		header: http.Header{"Content-Type": {"application/octet-stream"}},
	})
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return true, nil
}

// ---- pull ----

// Pull downloads every file of repo@rev into dir and verifies each one
// against the SHA-256 the registry recorded. Files already present and
// correct are kept; interrupted downloads resume where they stopped.
func (c *Client) Pull(ctx context.Context, repo, rev, dir string, progress func(path string, size int64, fetched bool)) (Revision, error) {
	revision, err := c.Revision(ctx, repo, rev)
	if err != nil {
		return Revision{}, err
	}
	for _, f := range revision.Files {
		dst, err := safeJoin(dir, f.Path)
		if err != nil {
			return Revision{}, err
		}
		fetched := false
		if !f.matches(dst) {
			if err := c.download(ctx, revision, f, dst); err != nil {
				return Revision{}, fmt.Errorf("download %s: %w", f.Path, err)
			}
			fetched = true
		}
		if progress != nil {
			progress(f.Path, f.Size, fetched)
		}
	}
	return revision, nil
}

// safeJoin places a repository path under dir and refuses anything that
// would land outside it.
func safeJoin(dir, repoPath string) (string, error) {
	local := filepath.FromSlash(repoPath)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("refusing to write outside the target directory: %q", repoPath)
	}
	return filepath.Join(dir, local), nil
}

// partSuffix names a download in progress, beside where the file will be.
const partSuffix = ".part"

func (c *Client) download(ctx context.Context, rev Revision, f File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	part := dst + partSuffix
	out, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o640) //nolint:gosec // dst was checked by safeJoin.
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	// Re-hash whatever an earlier attempt left, then ask for the rest.
	h := sha256.New()
	have, err := io.Copy(h, out)
	if err != nil {
		return err
	}
	// restart drops what the part file holds, so that the file is taken
	// from its first byte.
	restart := func() error {
		h.Reset()
		if err := out.Truncate(0); err != nil {
			return err
		}
		_, err := out.Seek(0, io.SeekStart)
		return err
	}
	// A part that cannot be the start of this file is not worth keeping, and
	// failing over it would fail every later pull the same way: one that is
	// larger than the file, or as large and not its content.
	stale := have > f.Size
	if have == f.Size && have > 0 {
		if f.SHA256 != "" {
			stale = hex.EncodeToString(h.Sum(nil)) != f.SHA256
		} else {
			stale = !f.matches(part)
		}
	}
	if stale {
		have = 0
		if err := restart(); err != nil {
			return err
		}
	}
	if have < f.Size {
		if err := c.fetchRange(ctx, rev, f, have, out, h, restart); err != nil {
			if errors.Is(err, errOversized) {
				// Nothing of an answer like that is worth resuming from.
				_ = out.Close()
				_ = os.Remove(part)
			}
			return err
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); f.SHA256 != "" && got != f.SHA256 {
		_ = out.Close()
		_ = os.Remove(part)
		return fmt.Errorf("integrity check failed: expected sha256 %s, got %s", f.SHA256, got)
	}
	if f.SHA256 == "" {
		// The registry had not fetched this file when it was listed, so it
		// could only name it by git blob id. Check against that.
		if err := out.Sync(); err != nil {
			return err
		}
		if !f.matches(part) {
			_ = out.Close()
			_ = os.Remove(part)
			return fmt.Errorf("integrity check failed: content does not match git blob id %s", f.GitOID)
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(part, dst)
}

// fetchRange downloads f from offset to its end into out, which already
// holds the bytes before offset, and adds what it writes to h.
//
// A registry may answer a request for the rest of a file with the whole of
// it (200 where 206 was asked for): a server or proxy that ignores Range,
// or this registry for a file whose earlier bytes it does not vouch for.
// That body is never added to what is there. restart empties out and h, and
// the whole file is taken from its first byte.
func (c *Client) fetchRange(ctx context.Context, rev Revision, f File, offset int64, out io.Writer, h hash.Hash, restart func() error) error {
	req := get("/" + kit.EscapePath(rev.Key()) + "/resolve/" + rev.SHA + "/" + kit.EscapePath(f.Path))
	if offset > 0 {
		req.header = http.Header{"Range": {fmt.Sprintf("bytes=%d-", offset)}}
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the registry answered the request for the rest of the file with status %d; "+
				"delete the %s file beside where it goes and pull again", resp.StatusCode, partSuffix)
		}
		if err := restart(); err != nil {
			return err
		}
		offset = 0
	}
	// The registry said how large the file is. Take that much and one byte
	// more: enough to tell that it is sending too much, without writing
	// whatever it sends to disk until the disk is full.
	want := f.Size - offset
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, want+1))
	if err != nil {
		return err
	}
	if n > want {
		return fmt.Errorf("%w: it lists this file as %d bytes. The download was stopped and discarded; "+
			"check that %s is the registry you meant, and run `modelctl verify` against it if it is yours", errOversized, f.Size, c.Endpoint)
	}
	return nil
}

// errOversized reports a download that kept going past the file's size.
var errOversized = errors.New("the registry sent more data than the size it lists")
