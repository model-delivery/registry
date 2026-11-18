// SPDX-License-Identifier: Apache-2.0

// Package mirror imports repositories from a Hugging Face compatible hub
// into the local registry, keeping upstream revision ids and verifying
// every file against the digests the hub publishes.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultEndpoint is the hub mirrored from unless configured otherwise.
const DefaultEndpoint = "https://huggingface.co"

// Errors describing why the upstream refused.
var (
	ErrUpstreamNotFound = errors.New("not found upstream")
	ErrUpstreamAuth     = errors.New("upstream denied access")
)

// Upstream talks to a hub's public HTTP API.
type Upstream struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

func (u *Upstream) endpoint() string {
	if u.Endpoint == "" {
		return DefaultEndpoint
	}
	return strings.TrimRight(u.Endpoint, "/")
}

func (u *Upstream) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return http.DefaultClient
}

// Info is the part of a hub's model description the mirror relies on, plus
// the full document for provenance.
type Info struct {
	ID  string
	SHA string
	Raw json.RawMessage
}

// Entry is one file in an upstream revision.
type Entry struct {
	Path string
	Size int64
	// OID is the git object id the hub lists for the file.
	OID string
	// LFSSHA256 is the content digest for LFS-tracked files, empty otherwise.
	LFSSHA256 string
}

func (u *Upstream) get(ctx context.Context, rawURL string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "model-delivery-registry")
	// net/http drops this header by itself when a redirect leaves the host,
	// so the token never reaches the hub's CDN.
	if u.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Token)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach upstream: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(resp.Header.Get("X-Error-Message"))
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint := "the repository is private or gated; supply an upstream token that has access"
		if u.Token != "" {
			hint = "the upstream token was rejected or lacks access to this repository"
		}
		return nil, fmt.Errorf("%w (%d): %s", ErrUpstreamAuth, resp.StatusCode, hint)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrUpstreamNotFound, detail)
	default:
		return nil, fmt.Errorf("upstream returned %d: %s", resp.StatusCode, detail)
	}
}

// Info resolves a revision (branch, tag or revision id) upstream.
func (u *Upstream) Info(ctx context.Context, repo, revision string) (Info, error) {
	resp, err := u.get(ctx, u.endpoint()+"/api/models/"+repo+"/revision/"+url.PathEscape(revision), nil)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return Info{}, fmt.Errorf("read upstream model info: %w", err)
	}
	var doc struct {
		ID  string `json:"id"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Info{}, fmt.Errorf("parse upstream model info: %w", err)
	}
	if doc.ID == "" || doc.SHA == "" {
		return Info{}, errors.New("upstream model info has no id or revision")
	}
	return Info{ID: doc.ID, SHA: doc.SHA, Raw: raw}, nil
}

var nextLinkRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// Tree lists every file in a revision, following the hub's pagination.
func (u *Upstream) Tree(ctx context.Context, repo, sha string) ([]Entry, error) {
	next := u.endpoint() + "/api/models/" + repo + "/tree/" + url.PathEscape(sha) + "?recursive=true&expand=false"
	var out []Entry
	for pages := 0; next != ""; pages++ {
		if pages > 10000 {
			return nil, errors.New("upstream file listing does not end")
		}
		resp, err := u.get(ctx, next, nil)
		if err != nil {
			return nil, err
		}
		var page []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			OID  string `json:"oid"`
			LFS  *struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("parse upstream file listing: %w", err)
		}
		for _, e := range page {
			if e.Type != "file" {
				continue
			}
			entry := Entry{Path: e.Path, Size: e.Size, OID: e.OID}
			if e.LFS != nil {
				entry.LFSSHA256 = e.LFS.OID
				entry.Size = e.LFS.Size
			}
			out = append(out, entry)
		}
		next = ""
		if m := nextLinkRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			ref, err := resp.Request.URL.Parse(m[1])
			if err != nil {
				return nil, fmt.Errorf("parse upstream pagination link: %w", err)
			}
			next = ref.String()
		}
	}
	return out, nil
}

// Open streams a file's content. If the connection drops part-way the
// returned reader reconnects and continues from where it stopped.
func (u *Upstream) Open(ctx context.Context, repo, sha, path string) io.ReadCloser {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	fileURL := u.endpoint() + "/" + repo + "/resolve/" + sha + "/" + strings.Join(segs, "/")
	return &resumeReader{ctx: ctx, open: func(offset int64) (io.ReadCloser, error) {
		h := http.Header{"Accept-Encoding": {"identity"}}
		if offset > 0 {
			h.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, err := u.get(ctx, fileURL, h)
		if err != nil {
			return nil, err
		}
		if offset > 0 && resp.StatusCode != http.StatusPartialContent {
			// The server ignored the range and is sending the whole file
			// again: skip what we already have.
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("skip to resume offset: %w", err)
			}
		}
		return resp.Body, nil
	}}
}

const maxResumes = 5

type resumeReader struct {
	ctx     context.Context
	open    func(offset int64) (io.ReadCloser, error)
	cur     io.ReadCloser
	offset  int64
	resumes int
	backoff time.Duration
}

func (r *resumeReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			body, err := r.open(r.offset)
			if err != nil {
				if !r.retryable(err) {
					return 0, err
				}
				continue
			}
			r.cur = body
		}
		n, err := r.cur.Read(p)
		r.offset += int64(n)
		if err == nil || errors.Is(err, io.EOF) {
			return n, err
		}
		_ = r.cur.Close()
		r.cur = nil
		if !r.retryable(err) {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
	}
}

// retryable decides whether to reconnect after err, waiting first if so.
func (r *resumeReader) retryable(err error) bool {
	if r.ctx.Err() != nil || r.resumes >= maxResumes ||
		errors.Is(err, ErrUpstreamNotFound) || errors.Is(err, ErrUpstreamAuth) {
		return false
	}
	r.resumes++
	wait := r.backoff
	if wait == 0 {
		wait = time.Duration(r.resumes) * 500 * time.Millisecond
	}
	select {
	case <-r.ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

func (r *resumeReader) Close() error {
	if r.cur == nil {
		return nil
	}
	err := r.cur.Close()
	r.cur = nil
	return err
}
