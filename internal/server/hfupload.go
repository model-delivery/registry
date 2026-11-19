// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/kit"
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
// A dataset is uploaded the same way, through the same handlers, at the
// Hub's dataset addresses: "type": "dataset" in the create request,
// /api/datasets/{repo}/... and /datasets/{repo}.git/... ; each handler is
// given the repository's key, which carries the type.
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
	// commitReadBuffer is the buffer a commit request is read through. A
	// line longer than it is still read whole, up to maxCommitLine.
	commitReadBuffer = 1 << 20
	// uploadURLTTL is how long a signed upload URL is valid. It has to
	// outlast the upload of the largest file, which starts after it is issued.
	uploadURLTTL = 12 * time.Hour
	// uploadKeyBytes is the length of the key upload URLs are signed with.
	uploadKeyBytes = 32
)

// How the Hub tells a client to send a file, in the preupload answer.
const (
	uploadModeInline = "regular" // inside the commit request
	uploadModeLFS    = "lfs"     // through the large-file protocol
)

const xetHint = "this registry does not support Xet uploads: set HF_HUB_DISABLE_XET=1 in the client's environment and run the upload again"

func newUploadKey() []byte {
	key := make([]byte, uploadKeyBytes)
	if _, err := rand.Read(key); err != nil {
		panic(err) // the system's random source is unavailable
	}
	return key
}

func (s *Server) hfCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
		Type         string `json:"type"`
	}
	if err := s.readJSON(w, r, maxSmallJSONBytes, &req, false); err != nil {
		hfError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	typ, err := registry.ParseRepoType(req.Type)
	if err != nil {
		hfError(w, http.StatusBadRequest, "this registry stores models and datasets, not "+req.Type+"s")
		return
	}
	namespace := req.Organization
	if namespace == "" {
		// The Hub puts a repository named without a namespace under the
		// caller's own. Here the caller is a token, so its name stands in.
		namespace = s.identity(r).Name
	}
	name := namespace + "/" + req.Name
	if err := registry.ValidateRepoName(name); err != nil {
		hfError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a valid repository id: use namespace/name with letters, digits, '.', '_' or '-'", name))
		return
	}
	repo := registry.RepoKey(typ, name)
	created, err := s.reg.CreateRepo(r.Context(), repo)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	// The repository's address on the Hub, which is its key: the client
	// reads the type back from it.
	address := baseURL(r) + "/" + repo
	if !created {
		// Clients that pass exist_ok treat 409 as success. The body names
		// the repository that exists, as the Hub's does.
		writeHFError(w, http.StatusConflict, "", "this repository already exists", map[string]string{"url": address})
		return
	}
	s.log.Info("repository created", "repo", repo)
	writeJSON(w, http.StatusOK, map[string]string{"url": address, "name": name, "id": name})
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
	if err := s.readJSON(w, r, maxJSONBytes, &req, false); err != nil {
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
		mode := uploadModeLFS
		if f.Size <= inlineUploadMax {
			mode = uploadModeInline
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
	case registry.ValidateRepoKey(repo) != nil:
		_, name := registry.SplitRepoKey(repo)
		hfError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a valid repository id: use namespace/name", name))
	case registry.IsSHA(rev):
		hfError(w, http.StatusBadRequest, "a revision id cannot be uploaded to: revisions never change. Upload to a branch such as main")
	default:
		return true
	}
	return false
}

// lfsError is why one object of a batch request cannot be uploaded, in the
// git-lfs batch protocol's shape.
type lfsError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// hfLFSBatch answers the git-lfs batch request: for each object the client
// wants to upload, either "already stored" or a signed URL to PUT it to.
func (s *Server) hfLFSBatch(w http.ResponseWriter, r *http.Request, repo string) {
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := s.readJSON(w, r, maxJSONBytes, &req, false); err != nil {
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
		Error   *lfsError         `json:"error,omitempty"`
	}
	// The URLs issued below are bound to the token that asked for them.
	tok := s.identity(r)
	out := make([]object, len(req.Objects))
	for i, o := range req.Objects {
		out[i] = object{OID: o.OID, Size: o.Size}
		if !blob.ValidDigest(o.OID) || o.Size < 0 {
			out[i].Error = &lfsError{http.StatusUnprocessableEntity, "the object id must be a lowercase hex SHA-256 and the size must not be negative"}
			continue
		}
		if over, err := s.blobTooLarge(o.Size); over {
			out[i].Error = &lfsError{http.StatusRequestEntityTooLarge, err.Error()}
			continue
		}
		// Asking about a blob marks it in use, so garbage collection leaves
		// it alone until this upload's commit arrives.
		info, stored, err := s.reg.TouchBlob(r.Context(), o.OID)
		if err != nil {
			s.hfFail(w, r, err)
			return
		}
		if stored && info.Size == o.Size {
			continue // no actions: the client skips the upload
		}
		expires := s.now().Add(uploadURLTTL).Unix()
		out[i].Actions = map[string]action{"upload": {
			Href: fmt.Sprintf("%s/api/lfs/upload/%s?size=%d&expires=%d&token=%s&signature=%s",
				baseURL(r), o.OID, o.Size, expires, url.QueryEscape(tok.Name), s.signUpload(o.OID, o.Size, expires, tok.Name)),
			ExpiresIn: int(uploadURLTTL.Seconds()),
		}}
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": out})
}

// signUpload authorises one upload: this digest, this size, until expires,
// on behalf of the token called tokenName. The client sends the file to the
// signed URL without its token (on the Hub that URL is the object store's),
// so the URL itself carries the grant. Naming the token in it lets the
// grant end when the token does. The name comes last in the signed text,
// so nothing in it can pass for another field.
func (s *Server) signUpload(oid string, size, expires int64, tokenName string) string {
	mac := hmac.New(sha256.New, s.uploadKey)
	fmt.Fprintf(mac, "lfs-upload\n%s\n%d\n%d\n%s", oid, size, expires, tokenName)
	return hex.EncodeToString(mac.Sum(nil))
}

// hfLFSUpload receives one large file. No role is asked of the request:
// the signed URL is the grant, and everything it rests on is checked here.
func (s *Server) hfLFSUpload(w http.ResponseWriter, r *http.Request) {
	oid := r.PathValue("sha256")
	q := r.URL.Query()
	size, errSize := strconv.ParseInt(q.Get("size"), 10, 64)
	expires, errExp := strconv.ParseInt(q.Get("expires"), 10, 64)
	given, errSig := hex.DecodeString(q.Get("signature"))
	tokenName := q.Get("token")
	want, _ := hex.DecodeString(s.signUpload(oid, size, expires, tokenName))
	if errSize != nil || errExp != nil || errSig != nil || !hmac.Equal(given, want) {
		hfError(w, http.StatusForbidden, "this upload URL is not valid: ask the registry for a new one")
		return
	}
	if s.now().Unix() > expires {
		hfError(w, http.StatusForbidden, "this upload URL has expired: start the upload again")
		return
	}
	// The URL is only as good as the token it was issued to: revoking the
	// token, or taking the write role from it, ends its uploads in flight.
	tok, err := s.reg.Token(r.Context(), tokenName)
	switch {
	case errors.Is(err, registry.ErrTokenNotFound), err == nil && !tok.Role.Allows(registry.RoleWrite):
		hfError(w, http.StatusForbidden, "the access token this upload URL was issued to has been revoked or no longer has the write role: start the upload again with a current token")
		return
	case err != nil:
		s.hfFail(w, r, err)
		return
	}
	if over, err := s.blobTooLarge(size); over {
		s.hfFail(w, r, err)
		return
	}
	// Streamed to storage and hashed on the way; rejected unless it is
	// exactly the bytes the URL was issued for.
	info, err := s.reg.PutBlob(r.Context(), r.Body, blob.Expect{SHA256: oid, Size: size})
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	s.uploaded(info)
	w.WriteHeader(http.StatusOK)
}

// commitLine is one line of a commit request.
type commitLine struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// The kinds of line in a commit request.
const (
	commitHeader        = "header"
	commitFile          = "file"
	commitLFSFile       = "lfsFile"
	commitDeletedFile   = "deletedFile"
	commitDeletedFolder = "deletedFolder"
)

func (s *Server) hfCommit(w http.ResponseWriter, r *http.Request, repo, rev string) {
	if !s.uploadTargetOK(w, r, repo, rev) {
		return
	}
	in := registry.CommitInput{Repo: repo, Branch: rev, Source: registry.SourcePush, Merge: true}
	// Each line is bounded below; this bounds how many there can be.
	r.Body = http.MaxBytesReader(baseWriter(w), r.Body, s.maxCommitBytes())
	br := bufio.NewReaderSize(r.Body, commitReadBuffer)
	sawHeader := false
	for {
		line, err := readLine(br, maxCommitLine)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.As(err, new(*http.MaxBytesError)) {
			hfError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"the commit is larger than %s in total: split the upload into several commits (upload_large_folder does), or send the larger files through the large-file upload, which this limit does not count",
				kit.HumanBytes(s.maxCommitBytes())))
			return
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
		if err := s.applyCommitLine(r.Context(), &in, entry); err != nil {
			s.hfFail(w, r, err)
			return
		}
		sawHeader = sawHeader || entry.Key == commitHeader
	}
	if !sawHeader {
		hfError(w, http.StatusBadRequest, "the commit has no header line")
		return
	}
	c, err := s.reg.Commit(r.Context(), in)
	if errors.Is(err, registry.ErrConflict) {
		// What the Hub answers when the branch moved under the client.
		hfError(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	s.m.commits.With(sourcePush).Inc()
	s.log.Info("revision recorded", "repo", c.Key(), "sha", c.SHA, "branch", rev, "via", "hugging face upload")
	writeJSON(w, http.StatusOK, map[string]any{
		"success":    true,
		"commitOid":  c.SHA,
		"commitUrl":  baseURL(r) + "/" + repo + "/commit/" + c.SHA,
		"hookOutput": "",
	})
}

// invalidf is an error for something in a request that cannot be accepted.
func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", registry.ErrInvalid, fmt.Sprintf(format, a...))
}

// applyCommitLine folds one line of a commit request into the commit.
func (s *Server) applyCommitLine(ctx context.Context, in *registry.CommitInput, entry commitLine) error {
	switch entry.Key {
	case commitHeader:
		var h struct {
			Summary      string `json:"summary"`
			Description  string `json:"description"`
			ParentCommit string `json:"parentCommit"`
		}
		if err := json.Unmarshal(entry.Value, &h); err != nil {
			return invalidf("unreadable commit header")
		}
		in.Message = h.Summary
		if h.Description != "" {
			in.Message += "\n\n" + h.Description
		}
		in.ExpectParent = h.ParentCommit
	case commitFile:
		var f struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(entry.Value, &f); err != nil {
			return invalidf("unreadable file entry")
		}
		if f.Encoding != "base64" {
			return invalidf("file %s uses encoding %q; only base64 is supported", f.Path, f.Encoding)
		}
		content, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return invalidf("file %s is not valid base64", f.Path)
		}
		if over, err := s.blobTooLarge(int64(len(content))); over {
			return err
		}
		info, err := s.reg.PutBlob(ctx, bytes.NewReader(content), blob.Expect{Size: int64(len(content))})
		if err != nil {
			return err
		}
		s.uploaded(info)
		// A file sent inline is recorded the way the Hub records a file
		// kept in git: not as an LFS file, so clients identify it by its
		// git blob id. The id is not given here; Registry.Commit takes it
		// from the stored blob when a file names none.
		in.Files = append(in.Files, registry.File{Path: f.Path, SHA256: info.SHA256})
	case commitLFSFile:
		var f struct {
			Path string `json:"path"`
			Algo string `json:"algo"`
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		}
		if err := json.Unmarshal(entry.Value, &f); err != nil {
			return invalidf("unreadable large-file entry")
		}
		if f.Algo != "" && f.Algo != "sha256" {
			return invalidf("file %s is identified by %q; only sha256 is supported", f.Path, f.Algo)
		}
		info, stored, err := s.reg.StatBlob(ctx, f.OID)
		if err != nil {
			return err
		}
		if !stored {
			return fmt.Errorf("%w: %s (sha256 %s). If the client uploaded it through Xet, %s",
				registry.ErrBlobMissing, f.Path, f.OID, xetHint)
		}
		if info.Size != f.Size {
			return invalidf("file %s is declared as %d bytes but the stored content is %d bytes", f.Path, f.Size, info.Size)
		}
		in.Files = append(in.Files, registry.File{Path: f.Path, SHA256: f.OID, LFS: true})
	case commitDeletedFile, commitDeletedFolder:
		var d struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(entry.Value, &d); err != nil || d.Path == "" {
			return invalidf("unreadable delete entry")
		}
		path := strings.TrimSuffix(d.Path, "/")
		if entry.Key == commitDeletedFolder {
			path += "/"
		}
		in.Delete = append(in.Delete, path)
	default:
		return invalidf("commit operation %q is not supported", entry.Key)
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
	_ = s.readJSON(w, r, maxSmallJSONBytes, &req, false) // the body is optional
	repo, err := s.reg.GetRepo(r.Context(), repoName)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	commits, err := s.reg.Commits(r.Context(), repo)
	if err != nil {
		s.hfFail(w, r, err)
		return
	}
	if len(commits) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	from := req.StartingPoint
	if from == "" {
		from = defaultBranch
	}
	if err := s.reg.CreateBranch(r.Context(), repoName, branch, from); err != nil {
		s.hfFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
