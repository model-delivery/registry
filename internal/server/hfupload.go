// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// The routes in this file let Hugging Face clients upload: `hf upload`,
// upload_file, upload_folder and anything else built on create_commit.
// An upload is four steps, and the client drives all of them:
//
//	POST /api/repos/create                      make sure the repository exists
//	POST /api/models/{repo}/preupload/{rev}     ask how to send each file
//	POST /{repo}.git/info/lfs/objects/batch     ask where to send the large ones
//	PUT  <signed url from the batch answer>     send one large file
//	POST /api/models/{repo}/commit/{rev}        record the revision
//
// Files up to inlineUploadMax travel inside the commit request. Larger ones
// use the git-lfs batch protocol, which names every file by its SHA-256:
// exactly how the registry stores them, so each upload is verified against
// the digest the client declared.
//
// One limit is outside the registry's control. A client with the hf_xet
// package installed (the default) sends large files through Hugging Face's
// Xet storage protocol and does not ask the server first. The registry does
// not speak Xet, so those clients must set HF_HUB_DISABLE_XET=1. See
// docs/compatibility.md.

const (
	// inlineUploadMax is the largest file a client is told to send inside
	// the commit request rather than through the large-file protocol.
	inlineUploadMax = 10 << 20
	// maxCommitLine bounds one line of a commit request: an inline file is
	// base64, so a third larger than the file, plus its JSON wrapping.
	maxCommitLine = 2*inlineUploadMax + 1<<20
	// uploadURLTTL is how long a signed upload URL is valid. It has to
	// outlast the upload of the largest file, which starts after it is issued.
	uploadURLTTL = 12 * time.Hour
)

const xetHint = "this registry does not support Xet uploads: set HF_HUB_DISABLE_XET=1 in the client's environment and run the upload again"

func newUploadKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err) // the system's random source is unavailable
	}
	return key
}

// baseURL is how the client reached this server, for links sent back to it.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// hfError writes an error the way Hugging Face clients read one: the
// message in the body and in X-Error-Message.
func hfError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("X-Error-Message", msg)
	writeError(w, status, msg)
}

// requireRole is Server.need for handlers that pick their role by method.
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, minRole role) bool {
	granted, authenticated := s.allowed(r, minRole)
	switch {
	case granted:
		return true
	case authenticated:
		hfError(w, http.StatusForbidden, "this token does not have permission to upload; it needs the write role")
	default:
		w.Header().Set("WWW-Authenticate", `Bearer realm="registry"`)
		hfError(w, http.StatusUnauthorized, "uploading needs an access token with the write role: set HF_TOKEN")
	}
	return false
}

func (s *Server) hfCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Type         string `json:"type"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		hfError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Type != "" && req.Type != "model" {
		hfError(w, http.StatusBadRequest, "this registry stores model repositories only, not "+req.Type+"s")
		return
	}
	namespace := req.Organization
	if namespace == "" {
		// The Hub puts a repository named without a namespace under the
		// caller's own. Here the caller is a token, so its name stands in.
		_, tok := s.roleOf(r)
		namespace = tok.Name
	}
	repo := namespace + "/" + req.Name
	created, err := s.reg.CreateRepo(r.Context(), repo)
	if err != nil {
		if errors.Is(err, registry.ErrInvalid) {
			hfError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a valid repository id: use namespace/name with letters, digits, '.', '_' or '-'", repo))
			return
		}
		s.fail(w, r, err)
		return
	}
	url := baseURL(r) + "/" + repo
	if !created {
		// Clients that pass exist_ok treat 409 as success.
		w.Header().Set("X-Error-Message", "this repository already exists")
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this repository already exists", "url": url})
		return
	}
	s.log.Info("repository created", "repo", repo)
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "name": repo, "id": repo})
}

// hfValidateYAML accepts any model card. The Hub checks the card's
// metadata before an upload; the registry stores README.md as a file.
func (s *Server) hfValidateYAML(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"errors": []string{}, "warnings": []string{}})
}

func (s *Server) hfPreupload(w http.ResponseWriter, r *http.Request, repo, rev string) {
	var req struct {
		Files []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		hfError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if !s.uploadTargetOK(w, r, repo, rev) {
		return
	}
	type file struct {
		Path         string `json:"path"`
		UploadMode   string `json:"uploadMode"`
		ShouldIgnore bool   `json:"shouldIgnore"`
	}
	out := make([]file, len(req.Files))
	for i, f := range req.Files {
		mode := "lfs"
		if f.Size <= inlineUploadMax {
			mode = "regular"
		}
		out[i] = file{Path: f.Path, UploadMode: mode}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

// uploadTargetOK checks what every upload step shares: no pull requests,
// and a branch (not a revision id) to commit to.
func (s *Server) uploadTargetOK(w http.ResponseWriter, r *http.Request, repo, rev string) bool {
	switch {
	case r.URL.Query().Get("create_pr") != "":
		hfError(w, http.StatusBadRequest, "this registry has no pull requests: upload to a branch instead of passing create_pr")
	case registry.ValidateRepoName(repo) != nil:
		hfError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a valid repository id: use namespace/name", repo))
	case registry.IsSHA(rev):
		hfError(w, http.StatusBadRequest, "a revision id cannot be uploaded to: revisions never change. Upload to a branch such as main")
	default:
		return true
	}
	return false
}

// hfLFSBatch answers the git-lfs batch request: for each object the client
// wants to upload, either "already stored" or a signed URL to PUT it to.
func (s *Server) hfLFSBatch(w http.ResponseWriter, r *http.Request, repo string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireRole(w, r, roleWrite) {
		return
	}
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		hfError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Operation != "upload" {
		hfError(w, http.StatusNotImplemented, "only uploads use this endpoint; download files from /"+repo+"/resolve/<revision>/<path>")
		return
	}
	type action struct {
		Href      string            `json:"href"`
		Header    map[string]string `json:"header,omitempty"`
		ExpiresIn int               `json:"expires_in"`
	}
	type object struct {
		OID     string            `json:"oid"`
		Size    int64             `json:"size"`
		Actions map[string]action `json:"actions,omitempty"`
		Error   *map[string]any   `json:"error,omitempty"`
	}
	out := make([]object, len(req.Objects))
	for i, o := range req.Objects {
		out[i] = object{OID: o.OID, Size: o.Size}
		if !blob.ValidDigest(o.OID) || o.Size < 0 {
			out[i].Error = &map[string]any{"code": 422, "message": "the object id must be a lowercase hex SHA-256 and the size must not be negative"}
			continue
		}
		// Asking about a blob marks it in use, so garbage collection leaves
		// it alone until this upload's commit arrives.
		info, stored, err := s.reg.TouchBlob(r.Context(), o.OID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if stored && info.Size == o.Size {
			continue // no actions: the client skips the upload
		}
		expires := time.Now().Add(uploadURLTTL).Unix()
		out[i].Actions = map[string]action{"upload": {
			Href: fmt.Sprintf("%s/api/lfs/upload/%s?size=%d&expires=%d&signature=%s",
				baseURL(r), o.OID, o.Size, expires, s.signUpload(o.OID, o.Size, expires)),
			ExpiresIn: int(uploadURLTTL.Seconds()),
		}}
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": out})
}

// signUpload authorises one upload: this digest, this size, until expires.
// The client sends the file to the signed URL without its token (on the
// Hub that URL is the object store's), so the URL itself carries the grant.
func (s *Server) signUpload(oid string, size, expires int64) string {
	mac := hmac.New(sha256.New, s.uploadKey)
	fmt.Fprintf(mac, "lfs-upload\n%s\n%d\n%d", oid, size, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) hfLFSUpload(w http.ResponseWriter, r *http.Request) {
	oid := r.PathValue("sha256")
	q := r.URL.Query()
	size, errSize := strconv.ParseInt(q.Get("size"), 10, 64)
	expires, errExp := strconv.ParseInt(q.Get("expires"), 10, 64)
	given, errSig := hex.DecodeString(q.Get("signature"))
	want, _ := hex.DecodeString(s.signUpload(oid, size, expires))
	if errSize != nil || errExp != nil || errSig != nil || !hmac.Equal(given, want) {
		writeError(w, http.StatusForbidden, "this upload URL is not valid: ask the registry for a new one")
		return
	}
	if time.Now().Unix() > expires {
		writeError(w, http.StatusForbidden, "this upload URL has expired: start the upload again")
		return
	}
	// Streamed to storage and hashed on the way; rejected unless it is
	// exactly the bytes the URL was issued for.
	if _, err := s.reg.PutBlob(r.Context(), r.Body, blob.Expect{SHA256: oid, Size: size}); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// commitLine is one line of a commit request.
type commitLine struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func (s *Server) hfCommit(w http.ResponseWriter, r *http.Request, repo, rev string) {
	if !s.uploadTargetOK(w, r, repo, rev) {
		return
	}
	in := registry.CommitInput{Repo: repo, Branch: rev, Source: registry.SourcePush, Merge: true}
	br := bufio.NewReaderSize(r.Body, 1<<20)
	sawHeader := false
	for {
		line, err := readLine(br, maxCommitLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errLineTooLong) {
			hfError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"a file sent inside the commit is larger than %d MiB; files that size must use the large-file upload", inlineUploadMax>>20))
			return
		}
		if err != nil {
			hfError(w, http.StatusBadRequest, "could not read the commit: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry commitLine
		if err := json.Unmarshal(line, &entry); err != nil {
			hfError(w, http.StatusBadRequest, "the commit is not valid line-delimited JSON: "+err.Error())
			return
		}
		if err := s.applyCommitLine(r, &in, entry); err != nil {
			status, _ := statusFor(err)
			if status == http.StatusInternalServerError {
				s.fail(w, r, err)
				return
			}
			hfError(w, status, err.Error())
			return
		}
		sawHeader = sawHeader || entry.Key == "header"
	}
	if !sawHeader {
		hfError(w, http.StatusBadRequest, "the commit has no header line")
		return
	}
	c, err := s.reg.Commit(r.Context(), in)
	if err != nil {
		status, _ := statusFor(err)
		if status == http.StatusInternalServerError {
			s.fail(w, r, err)
			return
		}
		if errors.Is(err, registry.ErrConflict) {
			// What the Hub answers when the branch moved under the client.
			status = http.StatusPreconditionFailed
		}
		hfError(w, status, err.Error())
		return
	}
	s.log.Info("revision recorded", "repo", c.Repo, "sha", c.SHA, "branch", rev, "via", "hugging face upload")
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"commitOid":  c.SHA,
		"commitUrl":  baseURL(r) + "/" + repo + "/commit/" + c.SHA,
		"hookOutput": "",
	})
}

// applyCommitLine folds one line of a commit request into the commit.
func (s *Server) applyCommitLine(r *http.Request, in *registry.CommitInput, entry commitLine) error {
	invalid := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{registry.ErrInvalid}, a...)...)
	}
	switch entry.Key {
	case "header":
		var h struct {
			Summary      string `json:"summary"`
			Description  string `json:"description"`
			ParentCommit string `json:"parentCommit"`
		}
		if err := json.Unmarshal(entry.Value, &h); err != nil {
			return invalid("unreadable commit header")
		}
		in.Message = h.Summary
		if h.Description != "" {
			in.Message += "\n\n" + h.Description
		}
		in.ExpectParent = h.ParentCommit
	case "file":
		var f struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(entry.Value, &f); err != nil {
			return invalid("unreadable file entry")
		}
		if f.Encoding != "base64" {
			return invalid("file %s uses encoding %q; only base64 is supported", f.Path, f.Encoding)
		}
		content, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return invalid("file %s is not valid base64", f.Path)
		}
		info, err := s.reg.PutBlob(r.Context(), bytes.NewReader(content), blob.Expect{Size: int64(len(content))})
		if err != nil {
			return err
		}
		// A file sent inline is recorded the way the Hub records a file
		// kept in git: identified by its git blob id.
		in.Files = append(in.Files, registry.File{Path: f.Path, SHA256: info.SHA256})
	case "lfsFile":
		var f struct {
			Path string `json:"path"`
			Algo string `json:"algo"`
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		}
		if err := json.Unmarshal(entry.Value, &f); err != nil {
			return invalid("unreadable large-file entry")
		}
		if f.Algo != "" && f.Algo != "sha256" {
			return invalid("file %s is identified by %q; only sha256 is supported", f.Path, f.Algo)
		}
		info, stored, err := s.reg.StatBlob(r.Context(), f.OID)
		if err != nil {
			return err
		}
		if !stored {
			return fmt.Errorf("%w: %s (sha256 %s). If the client uploaded it through Xet, %s",
				registry.ErrBlobMissing, f.Path, f.OID, xetHint)
		}
		if info.Size != f.Size {
			return invalid("file %s is declared as %d bytes but the stored content is %d bytes", f.Path, f.Size, info.Size)
		}
		in.Files = append(in.Files, registry.File{Path: f.Path, SHA256: f.OID, LFS: true})
	case "deletedFile", "deletedFolder":
		var d struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(entry.Value, &d); err != nil || d.Path == "" {
			return invalid("unreadable delete entry")
		}
		path := strings.TrimSuffix(d.Path, "/")
		if entry.Key == "deletedFolder" {
			path += "/"
		}
		in.Delete = append(in.Delete, path)
	default:
		return invalid("commit operation %q is not supported", entry.Key)
	}
	return nil
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line of any length up to limit, without its newline.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return nil, errLineTooLong
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		default:
			return nil, err
		}
	}
}

// hfCreateBranch backs create_branch, which `hf upload --revision` calls
// when the branch is new. A repository with no revision yet has nothing to
// branch from: the branch then comes into being with its first commit.
func (s *Server) hfCreateBranch(w http.ResponseWriter, r *http.Request, repoName, branch string) {
	var req struct {
		StartingPoint string `json:"startingPoint"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	repo, err := s.reg.GetRepo(r.Context(), repoName)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	commits, err := s.reg.Commits(r.Context(), repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(commits) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	from := req.StartingPoint
	if from == "" {
		from = "main"
	}
	if err := s.reg.CreateBranch(r.Context(), repoName, branch, from); err != nil {
		status, _ := statusFor(err)
		if status == http.StatusInternalServerError {
			s.fail(w, r, err)
			return
		}
		hfError(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
