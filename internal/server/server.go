// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package server is the HTTP face of the registry. It serves three things
// from one handler: the Hugging Face compatible routes existing clients
// call (hf.go), the registry's own REST API (api.go) and the web UI (ui.go).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/model-delivery/registry/internal/blob"
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
	// BackupKeep is how many stored metadata backups to keep when one is
	// taken through the API; 0 keeps them all.
	BackupKeep int
	// UpstreamClient overrides the HTTP client used to reach the upstream.
	UpstreamClient *http.Client
	Version        string
	Logger         *slog.Logger
}

// Server holds the handler's dependencies.
type Server struct {
	reg    *registry.Registry
	cfg    Config
	log    *slog.Logger
	puller *puller // nil unless pull-through caching is on
	// uploadKey signs the URLs large files are uploaded to. It is made at
	// start and never stored, so a restart invalidates URLs in flight; the
	// client then asks for new ones.
	uploadKey []byte
}

// New builds the HTTP handler.
func New(reg *registry.Registry, cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Upstream == "" {
		cfg.Upstream = mirror.DefaultEndpoint
	}
	s := &Server{reg: reg, cfg: cfg, log: cfg.Logger, uploadKey: newUploadKey()}
	if cfg.PullThrough {
		s.puller = newPuller(reg, s.upstream, cfg.PullThroughAllow, cfg.PullThroughMaxFileBytes, cfg.Logger)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": cfg.Version})
	})

	// Registry API.
	mux.Handle("GET /api/v1/repos", s.need(roleRead, s.apiListRepos))
	mux.Handle("GET /api/v1/repos/{ns}/{name}", s.need(roleRead, s.apiGetRepo))
	mux.Handle("GET /api/v1/repos/{ns}/{name}/revisions/{rev...}", s.need(roleRead, s.apiGetRevision))
	mux.Handle("POST /api/v1/repos/{ns}/{name}/commits", s.need(roleWrite, s.apiCommit))
	mux.Handle("DELETE /api/v1/repos/{ns}/{name}", s.need(roleAdmin, s.apiDeleteRepo))
	mux.Handle("HEAD /api/v1/blobs/{sha256}", s.need(roleWrite, s.apiStatBlob))
	mux.Handle("PUT /api/v1/blobs/{sha256}", s.need(roleWrite, s.apiPutBlob))
	mux.Handle("POST /api/v1/mirror", s.need(roleWrite, s.apiMirror))
	mux.Handle("GET /api/v1/stats", s.need(roleRead, s.apiStats))
	mux.Handle("POST /api/v1/gc", s.need(roleAdmin, s.apiGC))
	mux.Handle("GET /api/v1/backups", s.need(roleAdmin, s.apiListBackups))
	mux.Handle("POST /api/v1/backups", s.need(roleAdmin, s.apiBackupNow))
	mux.Handle("GET /api/v1/backup/snapshot", s.need(roleAdmin, s.apiSnapshot))
	mux.Handle("GET /api/v1/tokens", s.need(roleAdmin, s.apiListTokens))
	mux.Handle("POST /api/v1/tokens", s.need(roleAdmin, s.apiCreateToken))
	mux.Handle("DELETE /api/v1/tokens/{name}", s.need(roleAdmin, s.apiRevokeToken))

	// Hugging Face compatible routes. Their paths carry percent-encoded
	// revisions and repository ids of two segments, so they are parsed by
	// hand instead of by mux patterns.
	mux.Handle("/api/models/", s.need(roleRead, s.hfModels))
	mux.HandleFunc("GET /api/whoami-v2", s.hfWhoami)
	// Uploads from Hugging Face clients (hfupload.go).
	mux.Handle("POST /api/repos/create", s.need(roleWrite, s.hfCreateRepo))
	mux.Handle("POST /api/validate-yaml", s.need(roleRead, s.hfValidateYAML))
	mux.HandleFunc("PUT /api/lfs/upload/{sha256}", s.hfLFSUpload) // authorised by its signed URL
	mux.HandleFunc("/api/xet-is-not-supported/", func(w http.ResponseWriter, _ *http.Request) {
		hfError(w, http.StatusBadRequest, xetHint)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "this endpoint is not implemented by the registry")
	})

	// Web UI.
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS())))
	mux.HandleFunc("GET /ui/login", s.uiLoginForm)
	mux.HandleFunc("POST /ui/login", s.uiLogin)
	mux.HandleFunc("POST /ui/logout", s.uiLogout)
	mux.HandleFunc("GET /ui/repo/{ns}/{name}", s.uiRepo)
	mux.HandleFunc("POST /ui/repo/{ns}/{name}/delete", s.uiDeleteRepo)

	// Everything else: file downloads at /{ns}/{name}/resolve/{rev}/{path},
	// the UI home page, and hub-style repository links.
	mux.HandleFunc("/", s.root)

	// Browsers may only make state-changing requests from our own pages.
	// Non-browser clients (the CLI, Python libraries) are unaffected.
	return http.NewCrossOriginProtection().Handler(mux)
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	segs, ok := splitPath(r.URL.EscapedPath())
	switch {
	case !ok:
		writeError(w, http.StatusBadRequest, "malformed path")
	case len(segs) == 0:
		s.uiIndex(w, r)
	case len(segs) == 6 && strings.HasSuffix(segs[1], ".git") && strings.Join(segs[2:], "/") == "info/lfs/objects/batch":
		s.hfLFSBatch(w, r, segs[0]+"/"+strings.TrimSuffix(segs[1], ".git"))
	case len(segs) >= 5 && segs[2] == "resolve":
		s.need(roleRead, func(w http.ResponseWriter, r *http.Request) { s.hfResolve(w, r, segs) }).ServeHTTP(w, r)
	case len(segs) == 2 && r.Method == http.MethodGet:
		http.Redirect(w, r, "/ui/repo/"+url.PathEscape(segs[0])+"/"+url.PathEscape(segs[1]), http.StatusFound)
	default:
		http.NotFound(w, r)
	}
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
	return &mirror.Upstream{Endpoint: s.cfg.Upstream, Token: s.cfg.UpstreamToken, Client: s.cfg.UpstreamClient}
}

// ---- authentication ----

type role int

const (
	roleNone role = iota
	roleRead
	roleWrite
	roleAdmin
)

func roleFromName(name string) role {
	switch name {
	case registry.RoleAdmin:
		return roleAdmin
	case registry.RoleWrite:
		return roleWrite
	case registry.RoleRead:
		return roleRead
	}
	return roleNone
}

const cookieName = "md_token"

// presentedToken returns the secret a request carries, if any.
func presentedToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// roleOf reports what the request's token allows. A token this registry
// does not know grants nothing: people who switch HF_ENDPOINT often still
// send their Hugging Face token, and that must not break anonymous reads.
func (s *Server) roleOf(r *http.Request) (role, registry.Token) {
	tok, ok := s.reg.Authenticate(r.Context(), presentedToken(r))
	if !ok {
		return roleNone, registry.Token{}
	}
	return roleFromName(tok.Role), tok
}

// allowed applies the access rule shared by the API and the UI.
func (s *Server) allowed(r *http.Request, minRole role) (granted bool, authenticated bool) {
	got, _ := s.roleOf(r)
	if got >= minRole || (minRole == roleRead && s.cfg.AnonymousRead) {
		return true, got != roleNone
	}
	return false, got != roleNone
}

func (s *Server) need(minRole role, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		granted, authenticated := s.allowed(r, minRole)
		switch {
		case granted:
			h(w, r)
		case authenticated:
			writeError(w, http.StatusForbidden, "this token does not have permission for this operation")
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="registry"`)
			writeError(w, http.StatusUnauthorized, "a valid access token is required")
		}
	})
}

// ---- responses ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// statusFor maps registry errors to HTTP statuses and the error codes
// Hugging Face clients look for in X-Error-Code.
func statusFor(err error) (int, string) {
	if errors.As(err, new(errUpstream)) {
		return http.StatusBadGateway, ""
	}
	switch {
	case errors.Is(err, errPullBusy):
		return http.StatusServiceUnavailable, ""
	case errors.Is(err, registry.ErrRepoNotFound):
		return http.StatusNotFound, "RepoNotFound"
	case errors.Is(err, registry.ErrRevisionNotFound):
		return http.StatusNotFound, "RevisionNotFound"
	case errors.Is(err, registry.ErrEntryNotFound), errors.Is(err, blob.ErrNotFound):
		return http.StatusNotFound, "EntryNotFound"
	case errors.Is(err, registry.ErrTokenNotFound):
		return http.StatusNotFound, ""
	case errors.Is(err, registry.ErrInvalid), errors.Is(err, registry.ErrBlobMissing), errors.Is(err, blob.ErrMismatch):
		return http.StatusBadRequest, ""
	case errors.Is(err, registry.ErrConflict):
		return http.StatusConflict, ""
	case errors.Is(err, mirror.ErrUpstreamNotFound):
		return http.StatusNotFound, ""
	case errors.Is(err, mirror.ErrUpstreamAuth):
		return http.StatusBadGateway, ""
	case errors.Is(err, context.Canceled):
		return 499, ""
	}
	return http.StatusInternalServerError, ""
}

// fail writes err as a JSON error. Unexpected errors are logged and hidden.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := statusFor(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		msg = "internal error"
	}
	if errors.Is(err, errPullBusy) {
		w.Header().Set("Retry-After", "60")
	}
	if code != "" {
		w.Header().Set("X-Error-Code", code)
		w.Header().Set("X-Error-Message", msg)
	}
	writeError(w, status, msg)
}
