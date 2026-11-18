// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	"bytes":   humanBytes,
	"short":   func(s string) string { return s[:min(len(s), 12)] },
	"ago":     ago,
	"date":    func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"repoURL": func(name string) string { return "/ui/repo/" + escapeRepo(name) },
	"fileURL": func(repo, sha, p string) string {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		return "/" + escapeRepo(repo) + "/resolve/" + sha + "/" + strings.Join(segs, "/")
	},
}

func escapeRepo(name string) string {
	ns, n, _ := strings.Cut(name, "/")
	return url.PathEscape(ns) + "/" + url.PathEscape(n)
}

var uiPages = func() map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, page := range []string{"index.html", "repo.html", "login.html", "error.html"} {
		out[page] = template.Must(template.New("layout.html").Funcs(uiFuncs).
			ParseFS(uiFiles, "templates/layout.html", "templates/"+page))
	}
	return out
}()

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func ago(t time.Time) string {
	d := time.Since(t)
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
var notices = map[string]string{
	"deleted":     "The repository was deleted, and the files nothing else uses were freed.",
	"not-deleted": "Nothing deleted: the name you typed did not match.",
}

type uiPage struct {
	Title   string
	Version string
	// Viewer is the name of the signed-in token, empty when anonymous.
	Viewer  string
	IsAdmin bool
	Host    string
	Flash   string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	rl, tok := s.roleOf(r)
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	err := uiPages[page].Execute(w, uiPage{
		Title: title, Version: s.cfg.Version, Viewer: tok.Name, IsAdmin: rl >= roleAdmin,
		Host: scheme + "://" + r.Host, Flash: notices[r.URL.Query().Get("notice")], Data: data,
	})
	if err != nil {
		s.log.Error("render page", "page", page, "error", err)
	}
}

// uiGate sends visitors who may not read to the sign-in page.
func (s *Server) uiGate(w http.ResponseWriter, r *http.Request) bool {
	if granted, _ := s.allowed(r, roleRead); granted {
		return true
	}
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	return false
}

func (s *Server) uiIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.uiGate(w, r) {
		return
	}
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
	if !s.uiGate(w, r) {
		return
	}
	name := repoParam(r)
	detail, err := s.repoDetail(r, name)
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
	if rev == "" && len(detail.Revisions) > 0 {
		rev = detail.Revisions[0].SHA
		for _, ref := range detail.Refs {
			if ref.Name == "main" {
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
	if rl, _ := s.roleOf(r); rl < roleAdmin {
		s.render(w, r, http.StatusForbidden, "login.html", "Sign in", map[string]any{
			"Error": "Deleting a repository needs an admin token.",
		})
		return
	}
	name := repoParam(r)
	// Typing the name guards against deleting the wrong repository.
	if s.formValue(w, r, "confirm") != name {
		http.Redirect(w, r, "/ui/repo/"+escapeRepo(name)+"?notice=not-deleted", http.StatusSeeOther)
		return
	}
	res, err := s.reg.DeleteRepo(r.Context(), name)
	if err != nil && res.Blobs == 0 {
		s.uiError(w, r, err)
		return
	}
	s.log.Info("repository deleted", "repo", name, "blobs_freed", res.Blobs, "bytes_freed", res.Bytes, "via", "ui")
	http.Redirect(w, r, "/?notice=deleted", http.StatusSeeOther)
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
	http.SetCookie(w, s.sessionCookie(r, secret, 30*24*3600))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.sessionCookie(r, "", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionCookie builds the sign-in cookie. It is never readable by scripts
// and never sent on requests from other sites. It is marked Secure whenever
// the request arrived over HTTPS; a registry on a home network is often
// plain HTTP, where a Secure cookie would make signing in impossible.
func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // Secure is set for HTTPS requests; see the comment above.
		Name: cookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
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
