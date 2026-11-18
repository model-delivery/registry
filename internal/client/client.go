// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package client is the Go client for a registry server. modelctl is a
// thin shell around it.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1 by definition.
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

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil && size >= 0 {
		req.ContentLength = size
	}
	for k, v := range header {
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
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

func (c *Client) json(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	size := int64(-1)
	header := http.Header{}
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, size = bytes.NewReader(raw), int64(len(raw))
		header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(ctx, method, path, body, size, header)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Raw performs a JSON request and returns the response document as is.
func (c *Client) Raw(ctx context.Context, method, path string, in any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.json(ctx, method, path, in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Download saves the response body of a GET to dst, which must not exist,
// and returns how many bytes were written.
func (c *Client) Download(ctx context.Context, path, dst string) (int64, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil, -1, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // a path the user named.
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return 0, err
	}
	return n, out.Close()
}

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

// gitBlobID is the id git gives a file's content.
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
	h := sha1.New() //nolint:gosec // git blob ids are SHA-1 by definition.
	fmt.Fprintf(h, "blob %d\x00", fi.Size())
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Revision is a revision and its files.
type Revision struct {
	Repo  string `json:"repo"`
	SHA   string `json:"sha"`
	Files []File `json:"files"`
}

// Revision resolves a branch, tag or revision id.
func (c *Client) Revision(ctx context.Context, repo, rev string) (Revision, error) {
	var out Revision
	err := c.json(ctx, http.MethodGet, "/api/v1/repos/"+repo+"/revisions/"+url.PathEscape(rev), nil, &out)
	return out, err
}

// MirrorEvent is one progress line from a mirror run.
type MirrorEvent struct {
	Event           string `json:"event"`
	Repo            string `json:"repo"`
	SHA             string `json:"sha"`
	Ref             string `json:"ref"`
	Path            string `json:"path"`
	Status          string `json:"status"`
	Size            int64  `json:"size"`
	Files           int    `json:"files"`
	Bytes           int64  `json:"bytes"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
	ReusedBytes     int64  `json:"reused_bytes"`
	Error           string `json:"error"`
}

// Mirror asks the registry to import repo@revision from its upstream and
// reports progress as it happens. It returns the final "done" event.
func (c *Client) Mirror(ctx context.Context, repo, revision, upstreamToken string, progress func(MirrorEvent)) (MirrorEvent, error) {
	raw, err := json.Marshal(map[string]string{"repo": repo, "revision": revision, "upstream_token": upstreamToken})
	if err != nil {
		return MirrorEvent{}, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/mirror", bytes.NewReader(raw), int64(len(raw)),
		http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return MirrorEvent{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e MirrorEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return MirrorEvent{}, fmt.Errorf("unreadable progress line from registry: %w", err)
		}
		switch e.Event {
		case "error":
			return MirrorEvent{}, errors.New(e.Error)
		case "done":
			return e, nil
		}
		if progress != nil {
			progress(e)
		}
	}
	if err := sc.Err(); err != nil {
		return MirrorEvent{}, fmt.Errorf("connection to registry lost during mirror: %w", err)
	}
	return MirrorEvent{}, errors.New("registry closed the connection before the mirror finished")
}

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
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
}

// Push uploads the files under dir and records them as a new revision.
// Files the registry already holds (by SHA-256) are not sent again.
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
	err = c.json(ctx, http.MethodPost, "/api/v1/repos/"+repo+"/commits", map[string]any{
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

func (c *Client) uploadBlob(ctx context.Context, p, digest string, size int64) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, "/api/v1/blobs/"+digest, nil, -1, nil)
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
	resp, err = c.do(ctx, http.MethodPut, "/api/v1/blobs/"+digest, f, size, http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return true, nil
}

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

func (c *Client) download(ctx context.Context, rev Revision, f File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	part := dst + ".part"
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
	if have > f.Size {
		have = 0
		h.Reset()
		if err := out.Truncate(0); err != nil {
			return err
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	if have < f.Size {
		if err := c.fetchRange(ctx, rev, f, have, out, h); err != nil {
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

func (c *Client) fetchRange(ctx context.Context, rev Revision, f File, offset int64, out io.Writer, h hash.Hash) error {
	segs := strings.Split(f.Path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	header := http.Header{}
	if offset > 0 {
		header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.do(ctx, http.MethodGet, "/"+rev.Repo+"/resolve/"+rev.SHA+"/"+strings.Join(segs, "/"), nil, -1, header)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if offset > 0 && resp.StatusCode != http.StatusPartialContent {
		return errors.New("registry did not honour the resume request")
	}
	_, err = io.Copy(io.MultiWriter(out, h), resp.Body)
	return err
}
