// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package server is the HTTP face of the registry. It serves three things
// from one handler: the Hugging Face compatible routes existing clients
// call (hf.go, hfupload.go), the registry's own REST API (api.go,
// backup.go) and the web UI (ui.go).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/mirror"
	"github.com/model-delivery/registry/internal/registry"
)

// Config is everything the handler needs besides the registry.
type Config struct {
	// AnonymousRead lets requests without a token browse and download.
	// Writes always need a token.
	AnonymousRead bool
	// Upstream is the hub mirrored from. It is fixed by the operator and
	// never taken from a request, so the server cannot be pointed at
	// arbitrary hosts.
	Upstream      string
	UpstreamToken string
	// RedirectDownloads answers file requests with a redirect to a signed
	// URL on the object store instead of streaming the bytes through the
	// registry. It only has an effect when the storage backend can sign
	// URLs, and clients must be able to reach the object store directly.
	RedirectDownloads bool
	// PullThrough fetches a revision from the upstream the first time a
	// client asks for one the registry does not hold, then serves it.
	PullThrough bool
	// PullThroughAllow limits pull-through to repositories matching one of
	// these globs over the whole id ("meta-llama/*"). Empty allows all.
	PullThroughAllow []string
	// PullThroughMaxFileBytes is the largest file pull-through fetches on
	// demand. A larger one is refused with an error naming `modelctl
	// mirror`. 0 means no limit.
	PullThroughMaxFileBytes int64
	// PullThroughNoStream turns off sending a file to clients while
	// pull-through is still fetching it (stream.go). With it set, nothing
	// of a file is sent until all of it is stored and verified, and the
	// first download of a large file waits for the whole fetch.
	PullThroughNoStream bool
	// MaxBlobBytes is the largest single file an upload may store, through
	// the registry API or the Hugging Face upload routes. 0 means no limit.
	// It does not apply to files mirrored or pulled through from upstream.
	MaxBlobBytes int64
	// MaxCommitBytes bounds the whole body of a Hugging Face commit request,
	// which carries the small files of an upload. 0 means the default (see
	// limits.go).
	MaxCommitBytes int64
	// BodyTimeout is how long a client may take to send the body of a small
	// JSON or form request. 0 means the default. File transfers have no
	// time limit.
	BodyTimeout time.Duration
	// BackupKeep is how many stored metadata backups to keep when one is
	// taken through the API; 0 keeps them all.
	BackupKeep int
	// UpstreamClient overrides the HTTP client used to reach the upstream.
	// The default one refuses redirects that lead to local and private
	// addresses; see mirror.Upstream.Client before replacing it.
	UpstreamClient *http.Client
	// Now is the clock the server reads; nil means time.Now. Tests set it
	// to move time: when a signed upload URL expires, how long an upstream
	// "not found" is remembered.
	Now     func() time.Time
	Version string
	Logger  *slog.Logger
}

// Server is the registry's HTTP handler, together with the work it does on
// its own account: pull-through fetches, which outlive the requests that
// start them. Call Shutdown before closing the registry.
type Server struct {
	reg     *registry.Registry
	cfg     Config
	log     *slog.Logger
	puller  *puller // nil unless pull-through caching is on
	handler http.Handler
	// patterns is every route registered, as the mux was given it. The
	// documents that list routes are checked against it
	// (TestAPIDocumentMatchesTheRoutes).
	patterns []string
	// stop ends the server's lifetime context, which every pull-through job
	// runs on.
	stop context.CancelFunc
	// uploadKey signs the URLs large files are uploaded to. It is made at
	// start and never stored, so a restart invalidates URLs in flight; the
	// client then asks for new ones.
	uploadKey []byte
	// m is what GET /metrics reports (metrics.go). Its zero value counts
	// nothing.
	m serverMetrics
}

// New builds the server. The result is the http.Handler to serve.
func New(reg *registry.Registry, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Upstream == "" {
		cfg.Upstream = mirror.DefaultEndpoint
	}
	// The server's lifetime: from here until Shutdown. Background work is
	// started on it, never on a request's context and never on none.
	lifetime, stop := context.WithCancel(context.Background())
	s := &Server{reg: reg, cfg: cfg, log: cfg.Logger, stop: stop, uploadKey: newUploadKey()}
	s.m.declare(cfg.Version, s.now())
	if cfg.PullThrough {
		s.puller = newPuller(reg, s.upstream, cfg.PullThroughAllow, cfg.PullThroughMaxFileBytes, cfg.Logger)
		s.puller.ctx, s.puller.now = lifetime, s.now
		s.puller.stream = !cfg.PullThroughNoStream
		s.puller.m = s.m.pull
	}

	// Every route is registered with the access it needs: gate, with a role
	// and the way a refusal is written, or public, with a comment saying why
	// no role is asked for. Handlers do not check roles themselves.
	mux := http.NewServeMux()
	handle := func(pattern string, h http.Handler) {
		s.patterns = append(s.patterns, pattern)
		mux.Handle(pattern, h)
	}
	// Says only that the process is up; probes carry no token.
	handle("GET /healthz", public(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": cfg.Version})
	}))
	// What the registry is doing, for Prometheus (metrics.go). It follows
	// read access, as browsing does: counts of what is stored say how much
	// a private registry holds. A scraper sends a token as a bearer token.
	handle("GET /metrics", s.gate(registry.RoleRead, asAPI, s.serveMetrics))

	// Registry API. Models live under /api/v1/repos, as they always have,
	// and datasets under /api/v1/datasets: the same routes and the same
	// handlers, given the type.
	api := func(minimum registry.Role, h http.HandlerFunc) http.Handler { return s.gate(minimum, asAPI, h) }
	for _, typ := range []registry.RepoType{registry.TypeModel, registry.TypeDataset} {
		root := apiRepoRoot(typ)
		handle("GET "+root, api(registry.RoleRead, s.apiListRepos(typ)))
		handle("GET "+root+"/{ns}/{name}", api(registry.RoleRead, s.apiGetRepo(typ)))
		handle("GET "+root+"/{ns}/{name}/revisions/{rev...}", api(registry.RoleRead, s.apiGetRevision(typ)))
		handle("POST "+root+"/{ns}/{name}/commits", api(registry.RoleWrite, s.apiCommit(typ)))
		handle("DELETE "+root+"/{ns}/{name}", api(registry.RoleAdmin, s.apiDeleteRepo(typ)))
	}
	handle("HEAD /api/v1/blobs/{sha256}", api(registry.RoleWrite, s.apiStatBlob))
	handle("PUT /api/v1/blobs/{sha256}", api(registry.RoleWrite, s.apiPutBlob))
	handle("POST /api/v1/mirror", api(registry.RoleWrite, s.apiMirror))
	handle("GET /api/v1/stats", api(registry.RoleRead, s.apiStats))
	handle("POST /api/v1/gc", api(registry.RoleAdmin, s.apiGC))
	handle("POST /api/v1/verify", api(registry.RoleAdmin, s.apiVerify))
	handle("GET /api/v1/backups", api(registry.RoleAdmin, s.apiListBackups))
	handle("POST /api/v1/backups", api(registry.RoleAdmin, s.apiBackupNow))
	handle("GET /api/v1/backups/snapshot", api(registry.RoleAdmin, s.apiSnapshot))
	// The address v0.3.0 shipped, kept so its clients go on working.
	handle("GET /api/v1/backup/snapshot", api(registry.RoleAdmin, s.apiSnapshot))
	handle("GET /api/v1/tokens", api(registry.RoleAdmin, s.apiListTokens))
	handle("POST /api/v1/tokens", api(registry.RoleAdmin, s.apiCreateToken))
	handle("DELETE /api/v1/tokens/{name}", api(registry.RoleAdmin, s.apiRevokeToken))

	// Hugging Face compatible routes. Their paths carry percent-encoded
	// revisions and repository ids of two segments, so they are parsed by
	// hand instead of by mux patterns: hfRepos does that for everything
	// under /api/models/ and /api/datasets/ and gates each route it finds
	// with its own role. Spaces fall through to the "not implemented" below.
	handle(hfModelsRoot, public(s.hfRepos))
	handle(hfDatasetsRoot, public(s.hfRepos))
	// Answers who a token is, so it authenticates by itself: 401 unless the
	// request carries a token this registry issued. No role is needed to
	// ask, and an open registry's anonymous read does not apply.
	handle("GET /api/whoami-v2", public(s.hfWhoami))
	// Uploads from Hugging Face clients (hfupload.go).
	handle("POST /api/repos/create", s.gate(registry.RoleWrite, asHF, s.hfCreateRepo))
	handle("POST /api/validate-yaml", s.gate(registry.RoleRead, asHF, s.hfValidateYAML))
	// Opened by the signed URL the batch route issued and by nothing else:
	// clients send the file here without their token, as they would to a
	// hub's object store. hfLFSUpload checks the signature, its expiry and
	// that the token it was issued to still has the write role.
	handle("PUT /api/lfs/upload/{sha256}", public(s.hfLFSUpload))
	// A fixed explanation and a fixed "not implemented": neither says
	// anything about what the registry holds.
	handle("/api/xet-is-not-supported/", public(func(w http.ResponseWriter, _ *http.Request) {
		hfError(w, http.StatusBadRequest, xetHint)
	}))
	handle("/api/", public(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "this endpoint is not implemented by the registry")
	}))

	// Web UI.
	// The stylesheet and icon, which the sign-in page needs too.
	handle("GET /static/", public(http.StripPrefix("/static/", http.FileServerFS(staticFS())).ServeHTTP))
	// Signing in and out have to work without a token; signing in grants
	// nothing unless the token typed into the form is valid.
	handle("GET /ui/login", public(s.uiLoginForm))
	handle("POST /ui/login", public(s.uiLogin))
	handle("POST /ui/logout", public(s.uiLogout))
	for _, typ := range []registry.RepoType{registry.TypeModel, registry.TypeDataset} {
		page := uiRepoRoot(typ)
		handle("GET "+page+"{ns}/{name}", s.gate(registry.RoleRead, asUI, s.uiRepo(typ)))
		handle("POST "+page+"{ns}/{name}/delete", s.gate(registry.RoleAdmin, asUI, s.uiDeleteRepo(typ)))
	}

	// Everything else: file downloads at /{ns}/{name}/resolve/{rev}/{path}
	// and /datasets/{ns}/{name}/resolve/..., the large-file batch, the UI
	// home page and hub-style repository links. Like hfRepos, root parses
	// the path and gates what it finds.
	handle("/", public(s.root))

	// Browsers may only make state-changing requests from our own pages.
	// Non-browser clients (the CLI, Python libraries) are unaffected.
	// Outermost, every request is counted (metrics.go), whatever becomes of it.
	s.handler = s.instrument(s.guardBodies(http.NewCrossOriginProtection().Handler(s.identify(mux))))
	return s
}

// ServeHTTP makes the server the handler to pass to http.Server.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Shutdown ends the work the server does on its own account and waits for
// it: pull-through fetches are cancelled, and Shutdown returns once every
// one of them has stopped, or with ctx's error if they have not by then.
// Requests are not its business; stop the http.Server for those.
//
// Call it before closing the registry. After it returns nil, nothing the
// server started is still reading or writing.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stop()
	if s.puller == nil {
		return nil
	}
	return s.puller.wait(ctx)
}

// now is the server's clock. Deadlines set on a connection are the one use
// of time that does not go through it: those are the operating system's.
func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// root routes what no mux pattern claims. It checks nothing itself: each
// route below is gated with its own role where it is chosen.
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	segs, ok := splitPath(r.URL.EscapedPath())
	if !ok {
		writeError(w, http.StatusBadRequest, "malformed path")
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	to := parseRootPath(segs)
	switch {
	case to.kind == rootHome && !read, to.kind == rootLFSBatch && r.Method != http.MethodPost:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	case to.kind == rootHome:
		s.gate(registry.RoleRead, asUI, s.uiIndex).ServeHTTP(w, r)
	case to.kind == rootLFSBatch:
		s.gate(registry.RoleWrite, asHF, func(w http.ResponseWriter, r *http.Request) { s.hfLFSBatch(w, r, to.key()) }).ServeHTTP(w, r)
	case to.kind == rootResolve:
		rev, file := to.rest[0], strings.Join(to.rest[1:], "/")
		s.gate(registry.RoleRead, asHF, func(w http.ResponseWriter, r *http.Request) { s.hfResolve(w, r, to.key(), rev, file) }).ServeHTTP(w, r)
	case to.kind == rootRepoLink && r.Method == http.MethodGet:
		// A hub-style link. It only points at the repository page, which is
		// gated, and says nothing about whether the repository exists.
		http.Redirect(w, r, uiRepoRoot(to.typ)+url.PathEscape(to.ns)+"/"+url.PathEscape(to.name), http.StatusFound) // each segment may itself hold an encoded slash
	default:
		http.NotFound(w, r)
	}
}

// rootKind is which of root's routes a path is.
type rootKind int

const (
	rootOther    rootKind = iota // none: not found
	rootHome                     // the web UI's home page, /
	rootResolve                  // a file: {repo}/resolve/{rev}/{path...}
	rootLFSBatch                 // the large-file batch: {repo}.git/info/lfs/objects/batch
	rootRepoLink                 // a hub-style link to a repository: {repo}
)

// rootPath is what a path that no mux pattern claims addresses. root and
// routeClass both read paths through parseRootPath, so that what is counted
// is what is served.
type rootPath struct {
	kind     rootKind
	typ      registry.RepoType
	ns, name string
	// rest is what follows "resolve": the revision, then the file's path.
	rest []string
}

// key is the addressed repository's key.
func (p rootPath) key() string { return registry.RepoKey(p.typ, p.ns+"/"+p.name) }

// parseRootPath reads decoded path segments the way the Hub lays out its
// addresses: a model at /{ns}/{name}/..., and a dataset at
// /datasets/{ns}/{name}/.... A model whose namespace is literally
// "datasets", recorded before that namespace was reserved, is still reached
// at its own addresses, except where the path also reads as a dataset's.
func parseRootPath(segs []string) rootPath {
	if len(segs) == 0 {
		return rootPath{kind: rootHome}
	}
	if segs[0] == kit.DatasetsSegment && len(segs) >= 3 {
		if p := parseRepoPath(registry.TypeDataset, segs[1:]); p.kind != rootOther {
			return p
		}
	}
	return parseRepoPath(registry.TypeModel, segs)
}

// parseRepoPath reads segs, which begin with a repository's namespace and
// name, as one of root's routes for a repository of type typ.
func parseRepoPath(typ registry.RepoType, segs []string) rootPath {
	switch {
	case len(segs) == 6 && strings.HasSuffix(segs[1], ".git") && strings.Join(segs[2:], "/") == "info/lfs/objects/batch":
		return rootPath{kind: rootLFSBatch, typ: typ, ns: segs[0], name: strings.TrimSuffix(segs[1], ".git")}
	case len(segs) >= 5 && segs[2] == "resolve":
		return rootPath{kind: rootResolve, typ: typ, ns: segs[0], name: segs[1], rest: segs[3:]}
	case len(segs) == 2:
		return rootPath{kind: rootRepoLink, typ: typ, ns: segs[0], name: segs[1]}
	}
	return rootPath{}
}

// splitPath splits an escaped URL path into decoded segments, so an
// encoded slash inside a segment (a revision like refs%2Fpr%2F1) survives.
func splitPath(escaped string) ([]string, bool) {
	trimmed := strings.Trim(escaped, "/")
	if trimmed == "" {
		return nil, true
	}
	parts := strings.Split(trimmed, "/")
	for i, p := range parts {
		dec, err := url.PathUnescape(p)
		if err != nil || dec == "" {
			return nil, false
		}
		parts[i] = dec
	}
	return parts, true
}

// upstream is the hub this registry mirrors from, with the operator's token.
func (s *Server) upstream() *mirror.Upstream {
	return &mirror.Upstream{Endpoint: s.cfg.Upstream, Token: s.cfg.UpstreamToken, Client: s.cfg.UpstreamClient, Log: s.log}
}

// isHTTPS reports whether the client reached the server over HTTPS, either
// directly or through a proxy that says so.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// baseURL is how the client reached this server, for links sent back to it.
func baseURL(r *http.Request) string {
	if isHTTPS(r) {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

// ---- authentication ----

const cookieName = "md_token"

// presentedToken returns the secret a request carries, if any.
func presentedToken(r *http.Request) string {
	const scheme = "bearer "
	if h := r.Header.Get("Authorization"); len(h) > len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
		return strings.TrimSpace(h[len(scheme):])
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// identityKey is the request context key under which identify leaves the
// token a request presented.
type identityKey struct{}

// identify looks up the token a request carries, once, before any handler
// runs, and leaves the answer in the request's context for identity.
//
// "No such token" and "the database could not be asked" are different
// answers. The first is an anonymous request. The second is the server's
// failure: it is logged and answered 500, not passed off as a 401 that
// tells every user their token is bad.
func (s *Server) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, _, err := s.reg.Authenticate(r.Context(), presentedToken(r))
		if err != nil {
			s.fail(w, r, fmt.Errorf("look up the request's access token: %w", err))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, tok)))
	})
}

// identity is the token the request presented; the zero Token, which has
// no role, when it presented none this registry knows. A token from
// elsewhere grants nothing: people who switch HF_ENDPOINT often still send
// their Hugging Face token, and that must not break anonymous reads.
func (s *Server) identity(r *http.Request) registry.Token {
	tok, identified := r.Context().Value(identityKey{}).(registry.Token)
	if !identified {
		// A handler reached without identify in front of it, which only a
		// test does. A lookup that fails here grants nothing.
		var err error
		if tok, _, err = s.reg.Authenticate(r.Context(), presentedToken(r)); err != nil {
			s.log.Error("look up the request's access token", "error", err)
			return registry.Token{}
		}
	}
	return tok
}

// errorStyle is how a route tells a caller it may not do what it asked.
type errorStyle int

const (
	// asAPI answers 401 or 403 with the registry API's JSON error.
	asAPI errorStyle = iota
	// asHF answers the same statuses and repeats the message in
	// X-Error-Message, which is where Hugging Face clients read it.
	asHF
	// asUI answers a browser: a visitor who may not read is sent to the
	// sign-in page, and one who may read but not do what was asked is shown
	// that page with the reason.
	asUI
)

// gate is the one place access is decided. It lets a request through to h
// when its token has at least the minimum role, or when the role is read
// and the registry is open to anonymous readers; otherwise it answers in
// the given style and h never runs.
func (s *Server) gate(minimum registry.Role, style errorStyle, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held := s.identity(r).Role
		if held.Allows(minimum) || (minimum == registry.RoleRead && s.cfg.AnonymousRead) {
			h(w, r)
			return
		}
		// Any role at all means the request carried a token of ours.
		s.refuse(w, r, minimum, style, held.Allows(registry.RoleRead))
	})
}

// public marks a route that asks for no role, so that every registration
// says how it is protected. The comment at each use says why none is asked.
func public(h http.HandlerFunc) http.Handler { return h }

// refuse answers a request the gate turned away.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, minimum registry.Role, style errorStyle, authenticated bool) {
	if style == asUI {
		if minimum == registry.RoleRead {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		s.render(w, r, http.StatusForbidden, "login.html", "Sign in", map[string]any{
			"Error": "That needs a token with the " + string(minimum) + " role.",
		})
		return
	}
	status, msg := http.StatusForbidden, "this token does not have permission for this operation"
	if !authenticated {
		status, msg = http.StatusUnauthorized, "a valid access token is required"
		w.Header().Set("WWW-Authenticate", `Bearer realm="registry"`)
	}
	if style == asAPI {
		writeError(w, status, msg)
		return
	}
	if minimum == registry.RoleWrite {
		// The write routes Hugging Face clients call are the upload steps;
		// say what the person running the upload has to change.
		msg = "this token does not have permission to upload; it needs the write role"
		if !authenticated {
			msg = "uploading needs an access token with the write role: set HF_TOKEN"
		}
	}
	hfError(w, status, msg)
}

// ---- responses ----

// Statuses and headers with no name in net/http.
const (
	// statusClientClosedRequest is the conventional status for a request
	// whose client went away before it was answered. Nobody receives it; it
	// is what the access log of a proxy in front shows.
	statusClientClosedRequest = 499
	// retryAfterBusy is how long a client is told to wait when the registry
	// cannot take on another fetch.
	retryAfterBusy = time.Minute
	// internalErrorMessage is all a client is told about a failure of the
	// server's own. The log has the rest.
	internalErrorMessage = "internal error"
)

// The values of X-Error-Code that huggingface_hub turns into its own
// exceptions (RepositoryNotFoundError and so on).
const (
	hfCodeRepoNotFound     = "RepoNotFound"
	hfCodeRevisionNotFound = "RevisionNotFound"
	hfCodeEntryNotFound    = "EntryNotFound"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// errInternal marks a failure as the server's own, whatever it wraps: it is
// answered 500 and logged in full, and the client is told only that
// something went wrong. Wrapping is for errors that would otherwise be
// classified by what is inside them.
type errInternal struct{ err error }

func (e errInternal) Error() string { return e.err.Error() }
func (e errInternal) Unwrap() error { return e.err }

// statusFor maps an error to its HTTP status and, where Hugging Face
// clients look for one in X-Error-Code, its error code.
func statusFor(err error) (int, string) {
	if errors.As(err, new(errInternal)) {
		return http.StatusInternalServerError, ""
	}
	if errors.As(err, new(errUpstream)) {
		return http.StatusBadGateway, ""
	}
	if errors.As(err, new(errTooLarge)) {
		return http.StatusRequestEntityTooLarge, ""
	}
	switch {
	case errors.Is(err, errPullBusy), errors.Is(err, errShuttingDown):
		return http.StatusServiceUnavailable, ""
	case errors.Is(err, registry.ErrRepoNotFound):
		return http.StatusNotFound, hfCodeRepoNotFound
	case errors.Is(err, registry.ErrRevisionNotFound):
		return http.StatusNotFound, hfCodeRevisionNotFound
	case errors.Is(err, registry.ErrEntryNotFound), errors.Is(err, blob.ErrNotFound):
		return http.StatusNotFound, hfCodeEntryNotFound
	case errors.Is(err, registry.ErrTokenNotFound):
		return http.StatusNotFound, ""
	case errors.Is(err, registry.ErrInvalid), errors.Is(err, registry.ErrBlobMissing), errors.Is(err, blob.ErrMismatch):
		return http.StatusBadRequest, ""
	case errors.Is(err, registry.ErrConflict):
		return http.StatusConflict, ""
	case errors.Is(err, mirror.ErrUpstreamNotFound):
		return http.StatusNotFound, ""
	case errors.Is(err, mirror.ErrUpstreamAuth), errors.Is(err, mirror.ErrUnsafeUpstream):
		return http.StatusBadGateway, ""
	case errors.Is(err, context.Canceled):
		return statusClientClosedRequest, ""
	}
	return http.StatusInternalServerError, ""
}

// failure decides what a client is told about err: the status, the error
// code if it has one, and the message. A failure of the server's own is
// logged in full here and its message replaced, so that no path, query or
// driver error reaches a client.
func (s *Server) failure(w http.ResponseWriter, r *http.Request, err error) (status int, code, msg string) {
	status, code = statusFor(err)
	msg = err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		msg = internalErrorMessage
	}
	if errors.Is(err, errPullBusy) || errors.Is(err, errShuttingDown) {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfterBusy.Seconds())))
	}
	return status, code, msg
}

// fail writes err as the registry API's JSON error. An error that has a
// code carries it, and its message, in the headers as well, since the
// registry API's revision lookup is also read by Hugging Face clients.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := s.failure(w, r, err)
	if code != "" {
		w.Header().Set("X-Error-Code", code)
		w.Header().Set("X-Error-Message", msg)
	}
	writeError(w, status, msg)
}

// hfFail writes err the way Hugging Face clients read one. Every Hugging
// Face route reports its errors through it or through hfError.
func (s *Server) hfFail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := s.failure(w, r, err)
	writeHFError(w, status, code, msg, nil)
}

// hfError writes a fixed message the way Hugging Face clients read one.
func hfError(w http.ResponseWriter, status int, msg string) {
	writeHFError(w, status, "", msg, nil)
}

// writeHFError is the one writer of errors on Hugging Face routes. The
// message goes in the body and always in X-Error-Message, which is what
// huggingface_hub shows; the code, when there is one, goes in X-Error-Code,
// which is what it decides the exception by. more is added to the body.
func writeHFError(w http.ResponseWriter, status int, code, msg string, more map[string]string) {
	if code != "" {
		w.Header().Set("X-Error-Code", code)
	}
	w.Header().Set("X-Error-Message", msg)
	body := map[string]string{"error": msg}
	for k, v := range more {
		body[k] = v
	}
	writeJSON(w, status, body)
}
