// Copyright 2026 Spectrum Labs LLC
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
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/kit"
)

// DefaultEndpoint is the hub mirrored from unless configured otherwise.
const DefaultEndpoint = "https://huggingface.co"

// Errors describing why the upstream refused.
var (
	ErrUpstreamNotFound = errors.New("not found upstream")
	ErrUpstreamAuth     = errors.New("upstream denied access")
	// ErrUnsafeUpstream reports that the upstream pointed the registry at
	// an address it does not follow: see guard.go.
	ErrUnsafeUpstream = errors.New("the upstream pointed at an address the registry does not follow")
)

// upstreamError marks a failure as the upstream's or the network's doing:
// it could not be reached, answered with an error, sent something that is
// not what it listed. It adds nothing to the message.
type upstreamError struct{ err error }

func (e upstreamError) Error() string { return e.err.Error() }
func (e upstreamError) Unwrap() error { return e.err }

// blame marks err as caused by the upstream. A request this side gave up
// on (a cancelled context) is nobody's failure and is left unmarked.
func blame(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || FromUpstream(err) {
		return err
	}
	return upstreamError{err}
}

// FromUpstream reports whether err was caused by the upstream or the
// network between: not found, access denied, an address the registry does
// not follow, a transport failure, a malformed answer, or content that did
// not match its digest. Everything else that can go wrong in a mirror is
// this registry's own failure (its disk, its database), and a caller that
// answers a client should not describe that as the upstream's.
func FromUpstream(err error) bool {
	return errors.As(err, new(upstreamError)) || errors.Is(err, ErrUpstreamNotFound) ||
		errors.Is(err, ErrUpstreamAuth) || errors.Is(err, ErrUnsafeUpstream)
}

// blamedBody is an upstream response body whose read errors are marked as
// the upstream's, so they can be told from a failure to store what was read.
type blamedBody struct{ io.ReadCloser }

func (b blamedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = blame(err)
	}
	return n, err
}

// Upstream talks to a hub's public HTTP API.
//
// The operator configures one address, Endpoint, and trusts it. Everything
// else the upstream can make the registry contact (a redirect, a link to
// the next page of a listing) is held to the rules in guard.go.
type Upstream struct {
	Endpoint string
	Token    string
	// Client overrides the HTTP client. The default refuses to connect to
	// local and private addresses other than the configured endpoint's; a
	// client supplied here does its own dialling, so that check is then
	// limited to addresses written as IP literals.
	Client *http.Client
	// Log receives what the upstream sent with an error answer, which is
	// kept out of the errors returned to callers. Nil discards it.
	Log *slog.Logger
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
	return guardedClient
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

// Bounds on what one upstream is allowed to make the registry do or hold.
const (
	// maxRedirects is how many redirects one request follows, as net/http does.
	maxRedirects = 10
	// maxListingPages is how many pages of a file listing are followed
	// before the listing is taken not to end.
	maxListingPages = 10000
	// maxInfoBytes is the most that is read of a model description.
	maxInfoBytes = 32 << 20
	// maxLoggedErrorBody is how much of an error answer's body is logged.
	maxLoggedErrorBody = 512
	// maxErrorDetail is how much of the hub's one-line reason is passed on.
	maxErrorDetail = 200
	// maxResumes is how many times one download reconnects after its
	// connection drops, and resumeDelay is the wait before the first of
	// them; each later one waits that much longer.
	maxResumes  = 5
	resumeDelay = 500 * time.Millisecond
)

// get fetches rawURL, following redirects itself, one hop at a time, so
// that each hop is checked before it is contacted:
//
//   - The token goes only to the configured endpoint's own scheme, host and
//     port. Go's client would also send it to any subdomain of the first
//     host a request went to; a hub's file storage is often exactly that,
//     and has no use for the token.
//   - A hop to any other host is refused if that host is a loopback,
//     private or link-local address (guard.go).
func (u *Upstream) get(ctx context.Context, rawURL string, header http.Header) (*http.Response, error) {
	home, err := url.Parse(u.endpoint())
	if err != nil || home.Host == "" {
		return nil, fmt.Errorf("the upstream address %q is not a URL", u.endpoint())
	}
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	client := *u.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for hop := 0; ; hop++ {
		trusted := sameOrigin(home, target)
		hopCtx := ctx
		if !trusted {
			if hopCtx, err = guardHop(ctx, target); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(hopCtx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", "model-delivery-registry")
		if u.Token != "" && trusted {
			req.Header.Set("Authorization", "Bearer "+u.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("reach upstream: %w", err)
		}
		if !isRedirect(resp.StatusCode) {
			return u.checked(ctx, resp, trusted)
		}
		next, err := resp.Location()
		_ = resp.Body.Close()
		switch {
		case err != nil:
			return nil, fmt.Errorf("upstream answered %d without saying where to go: %w", resp.StatusCode, err)
		case hop == maxRedirects:
			return nil, fmt.Errorf("upstream redirected more than %d times", maxRedirects)
		case next.Scheme != "http" && next.Scheme != "https":
			return nil, fmt.Errorf("%w: it redirected to a %q address", ErrUnsafeUpstream, next.Scheme)
		}
		target = next
	}
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// checked returns resp if it is a success, and otherwise the error it
// stands for. What the upstream wrote in an error body goes to the log and
// never into the error: the error travels back to whoever made the request,
// and the body could be another service's page.
func (u *Upstream) checked(ctx context.Context, resp *http.Response, trusted bool) (*http.Response, error) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxLoggedErrorBody))
	if u.Log != nil {
		level := slog.LevelWarn
		if resp.StatusCode == http.StatusNotFound {
			level = slog.LevelDebug // an everyday answer to a wrong name
		}
		at := *resp.Request.URL
		at.RawQuery, at.User = "", nil // a signed address carries its grant in the query
		u.Log.Log(ctx, level, "the upstream answered with an error", "status", resp.StatusCode, "url", at.String(), "body", string(body))
	}
	// The one-line reason a hub puts in this header is worth passing on,
	// when it is the configured hub that said it.
	detail := ""
	if trusted {
		detail = strings.TrimSpace(resp.Header.Get("X-Error-Message"))
		if len(detail) > maxErrorDetail {
			detail = detail[:maxErrorDetail]
		}
	}
	if detail != "" {
		detail = ": " + detail
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint := "the repository is private or gated; supply an upstream token that has access"
		if u.Token != "" {
			hint = "the upstream token was rejected or lacks access to this repository"
		}
		return nil, fmt.Errorf("%w (%d): %s", ErrUpstreamAuth, resp.StatusCode, hint)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w%s", ErrUpstreamNotFound, detail)
	default:
		return nil, fmt.Errorf("upstream returned %d%s", resp.StatusCode, detail)
	}
}

// info resolves a revision (branch, tag or revision id) upstream.
func (u *Upstream) info(ctx context.Context, repo, revision string) (Info, error) {
	resp, err := u.get(ctx, u.endpoint()+"/api/models/"+repo+"/revision/"+url.PathEscape(revision), nil)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxInfoBytes))
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

// tree lists every file in a revision, following the hub's pagination.
func (u *Upstream) tree(ctx context.Context, repo, sha string) ([]Entry, error) {
	home, err := url.Parse(u.endpoint())
	if err != nil {
		return nil, fmt.Errorf("the upstream address %q is not a URL", u.endpoint())
	}
	next := u.endpoint() + "/api/models/" + repo + "/tree/" + url.PathEscape(sha) + "?recursive=true&expand=false"
	var out []Entry
	for pages := 0; next != ""; pages++ {
		if pages > maxListingPages {
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
			// A listing continues where it began. The next page is an API
			// call, sent with the token, so it goes nowhere else.
			if !sameOrigin(home, ref) {
				return nil, fmt.Errorf("%w: its file listing continues on another host, %s", ErrUnsafeUpstream, ref.Host)
			}
			next = ref.String()
		}
	}
	return out, nil
}

// open streams a file's content. If the connection drops part-way the
// returned reader reconnects and continues from where it stopped.
func (u *Upstream) open(ctx context.Context, repo, sha, path string) io.ReadCloser {
	fileURL := u.endpoint() + "/" + repo + "/resolve/" + sha + "/" + kit.EscapePath(path)
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

type resumeReader struct {
	ctx     context.Context
	open    func(offset int64) (io.ReadCloser, error)
	cur     io.ReadCloser
	offset  int64
	resumes int
	// backoff, when set, replaces the growing wait between reconnections.
	// Only tests set it, to not spend seconds waiting.
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
		errors.Is(err, ErrUpstreamNotFound) || errors.Is(err, ErrUpstreamAuth) || errors.Is(err, ErrUnsafeUpstream) {
		return false
	}
	r.resumes++
	wait := r.backoff
	if wait == 0 {
		wait = time.Duration(r.resumes) * resumeDelay
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
