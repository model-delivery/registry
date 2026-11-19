// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/kit"
	"github.com/model-delivery/registry/internal/registry"
)

//go:embed templates/*.html static/*
var uiFiles embed.FS

func staticFS() fs.FS {
	sub, err := fs.Sub(uiFiles, "static")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

var uiFuncs = template.FuncMap{
	"bytes":   kit.HumanBytes,
	"short":   kit.Short,
	"date":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"repoURL": repoPage,
	"fileURL": func(repo, sha, p string) string {
		return "/" + kit.EscapePath(repo) + "/resolve/" + sha + "/" + kit.EscapePath(p)
	},
}

// repoPage is the address of a repository's page in the web UI.
func repoPage(name string) string { return "/ui/repo/" + kit.EscapePath(name) }

var uiPages = func() map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, page := range []string{"index.html", "repo.html", "login.html", "error.html"} {
		out[page] = template.Must(template.New("layout.html").Funcs(uiFuncs).
			ParseFS(uiFiles, "templates/layout.html", "templates/"+page))
	}
	return out
}()

// ago says how long before now t was, in words.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// notices are the messages one page can ask the next to show, after a
// redirect. The address carries a code (?notice=deleted) and the text comes
// from here, so a link cannot make the registry display words of someone
// else's choosing. An unknown code shows nothing.
// The codes of the notices (see notices).
const (
	noticeDeleted       = "deleted"
	noticeDeletedPartly = "deleted-partly"
	noticeNotDeleted    = "not-deleted"
)

var notices = map[string]string{
	noticeDeleted: "The repository was deleted. The files only it used were freed, " +
		"except any that an upload in progress has just sent; garbage collection removes those later if the upload does not use them.",
	noticeDeletedPartly: "The repository was deleted, but some of the files only it used could not be removed and still take up space. " +
		"The server log has the reason.",
	noticeNotDeleted: "Nothing deleted: the name you typed did not match.",
}

type uiPage struct {
	Title   string
	Version string
	// Viewer is the name of the signed-in token, empty when anonymous.
	Viewer  string
	IsAdmin bool
	Host    string
	Flash   string
	// Now is when the page was made, for times shown as "3 h ago".
	Now  time.Time
	Data any
}

// Ago says how long before the page was made t was.
func (p uiPage) Ago(t time.Time) string { return ago(p.Now, t) }

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	tok := s.identity(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	err := uiPages[page].Execute(w, uiPage{
		Title: title, Version: s.cfg.Version, Viewer: tok.Name, IsAdmin: tok.Role.Allows(registry.RoleAdmin),
		Host: baseURL(r), Flash: notices[r.URL.Query().Get("notice")], Now: s.now(), Data: data,
	})
	if err != nil {
		s.log.Error("render page", "page", page, "error", err)
	}
}

func (s *Server) uiIndex(w http.ResponseWriter, r *http.Request) {
	repos, err := s.reg.ListRepos(r.Context())
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	stats, err := s.reg.Stats(r.Context())
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "index.html", "Repositories", map[string]any{
		"Repos": repos, "Stats": stats, "Saved": stats.LogicalBytes - stats.StoredBytes,
	})
}

func (s *Server) uiRepo(w http.ResponseWriter, r *http.Request) {
	name := repoParam(r)
	detail, err := s.describeRepo(r.Context(), name)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	if len(detail.Revisions) == 0 {
		s.render(w, r, http.StatusOK, "error.html", name, map[string]any{
			"Error": "This repository has no revisions yet. Upload to it with `hf upload` or `modelctl push`.",
		})
		return
	}
	// Show the requested revision, else main, else the newest one.
	rev := r.URL.Query().Get("rev")
	if rev == "" {
		rev = detail.Revisions[0].SHA
		for _, ref := range detail.Refs {
			if ref.Name == defaultBranch {
				rev = ref.SHA
			}
		}
	}
	c, err := s.reg.Resolve(r.Context(), name, rev)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	files, err := s.reg.Files(r.Context(), c)
	if err != nil {
		s.uiError(w, r, err)
		return
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	names := map[string][]string{}
	for _, ref := range detail.Refs {
		names[ref.SHA] = append(names[ref.SHA], ref.Name)
	}
	// A few upstream fields worth showing for mirrored revisions.
	var upstream struct {
		PipelineTag string `json:"pipeline_tag"`
		LibraryName string `json:"library_name"`
		CardData    struct {
			License any `json:"license"`
		} `json:"cardData"`
	}
	if c.UpstreamInfo != "" {
		_ = json.Unmarshal([]byte(c.UpstreamInfo), &upstream)
	}
	license, _ := upstream.CardData.License.(string)
	s.render(w, r, http.StatusOK, "repo.html", name, map[string]any{
		"Repo": detail, "Commit": c, "Files": files, "Total": total, "RefNames": names,
		"Pipeline": upstream.PipelineTag, "Library": upstream.LibraryName, "License": license,
	})
}

func (s *Server) uiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	name := repoParam(r)
	// Typing the name guards against deleting the wrong repository.
	if s.formValue(w, r, "confirm") != name {
		http.Redirect(w, r, repoPage(name)+"?notice="+noticeNotDeleted, http.StatusSeeOther)
		return
	}
	res, err := s.reg.DeleteRepo(r.Context(), name)
	// As in apiDeleteRepo: once the deletion has committed the repository
	// is gone, whatever then happens to the files it held.
	notice := noticeDeleted
	switch {
	case errors.Is(err, registry.ErrBlobsNotFreed):
		s.logNotFreed(err, "repo", name)
		notice = noticeDeletedPartly
	case err != nil:
		s.uiError(w, r, err)
		return
	}
	s.log.Info("repository deleted", "repo", name, "blobs_freed", res.Blobs, "bytes_freed", res.Bytes, "via", "ui")
	http.Redirect(w, r, "/?notice="+notice, http.StatusSeeOther)
}

func (s *Server) uiLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "login.html", "Sign in", map[string]any{})
}

func (s *Server) uiLogin(w http.ResponseWriter, r *http.Request) {
	// Anyone can post here, so the body is bounded before it is parsed.
	secret := strings.TrimSpace(s.formValue(w, r, "token"))
	_, ok, err := s.reg.Authenticate(r.Context(), secret)
	if err != nil {
		// The database could not be asked: that is not a wrong token.
		s.uiError(w, r, fmt.Errorf("look up the access token: %w", err))
		return
	}
	if !ok {
		s.render(w, r, http.StatusUnauthorized, "login.html", "Sign in", map[string]any{
			"Error": "That token is not valid for this registry.",
		})
		return
	}
	http.SetCookie(w, sessionCookie(r, secret, sessionLifetime))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, sessionCookie(r, "", 0))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionLifetime is how long a browser stays signed in.
const sessionLifetime = 30 * 24 * time.Hour

// sessionCookie builds the sign-in cookie, to last for lifetime; none
// deletes it. It is never readable by scripts and never sent on requests
// from other sites. It is marked Secure whenever the request arrived over
// HTTPS; a registry on a home network is often plain HTTP, where a Secure
// cookie would make signing in impossible.
func sessionCookie(r *http.Request, value string, lifetime time.Duration) *http.Cookie {
	maxAge := int(lifetime.Seconds())
	if lifetime <= 0 {
		maxAge = -1 // tells the browser to delete the cookie now
	}
	return &http.Cookie{ //nolint:gosec // Secure is set for HTTPS requests; see the comment above.
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: isHTTPS(r),
	}
}

func (s *Server) uiError(w http.ResponseWriter, r *http.Request, err error) {
	status, _ := statusFor(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("ui request failed", "path", r.URL.Path, "error", err)
		msg = "Something went wrong on the server. The log has the details."
	}
	if status == http.StatusNotFound {
		msg = "Not found: " + msg
	}
	s.render(w, r, status, "error.html", http.StatusText(status), map[string]any{"Error": msg})
}
