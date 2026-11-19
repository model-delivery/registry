// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/model-delivery/registry/internal/kit"
)

// Bounds on what a request may send. File transfers have no time limit and,
// unless the operator sets one, no size limit: model files are large and
// uploads are long. Everything else is small and is held to that.

const (
	// maxFormBytes bounds the body of the web UI's two forms, which carry
	// one short field each. Without it a multipart body would be spooled
	// to the server's temporary directory in full.
	maxFormBytes = 64 << 10
	// maxSmallJSONBytes bounds a JSON body that names one thing: the
	// repository to create, the branch to start from.
	maxSmallJSONBytes = 1 << 20
	// maxJSONBytes bounds a JSON body that lists files: a commit through
	// the registry API, the files of an upload, the objects of a large-file
	// batch. A repository of a hundred thousand files fits.
	maxJSONBytes = 64 << 20
	// defaultMaxCommitBytes bounds the whole body of a Hugging Face commit
	// request: every file small enough to travel inside it, base64-encoded.
	// It is far more than a real upload sends (files over 10 MiB do not
	// count; they go through the large-file upload), and exists so the
	// request has an end.
	defaultMaxCommitBytes = 4 << 30
	// defaultBodyTimeout is how long a client may take to send a small JSON
	// or form body once its headers have arrived.
	defaultBodyTimeout = 30 * time.Second
)

func (s *Server) maxCommitBytes() int64 {
	if s.cfg.MaxCommitBytes > 0 {
		return s.cfg.MaxCommitBytes
	}
	return defaultMaxCommitBytes
}

func (s *Server) bodyTimeout() time.Duration {
	if s.cfg.BodyTimeout > 0 {
		return s.cfg.BodyTimeout
	}
	return defaultBodyTimeout
}

// guardBodies gives every request that announces a body the body timeout
// to send it, unless the request is a file transfer.
//
// The handlers that read a small body set this deadline themselves, and
// lift it (smallBody). This covers the rest: a route that reads no body, a
// request that is refused before its handler runs, a path that does not
// exist. The HTTP server reads what is left of a request body before it
// answers, so without a deadline a client that announces a body and never
// sends it holds its connection for as long as it likes, on any route and
// without a token. With one, that read gives up and the connection is
// closed after the answer.
//
// A request without a body is left alone: downloads and other long
// responses are not timed.
//
// The deadlines in this file are set from time.Now, not the server's
// clock: a connection's deadline is compared with the real time.
func (s *Server) guardBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 && !isFileTransfer(r) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(s.bodyTimeout()))
		}
		next.ServeHTTP(w, r)
	})
}

// isFileTransfer reports whether r is one of the requests whose body is
// model content and may take as long as it takes: an upload of one file
// (both upload routes are PUT, and nothing small is sent with PUT), or a
// Hugging Face commit, which carries an upload's small files.
func isFileTransfer(r *http.Request) bool {
	switch r.Method {
	case http.MethodPut:
		return true
	case http.MethodPost:
		// Parsed the way hfModels parses it: {ns}/{name}/commit/{rev}.
		rest, under := strings.CutPrefix(r.URL.EscapedPath(), "/api/models/")
		segs, ok := splitPath(rest)
		return under && ok && len(segs) == 4 && segs[2] == "commit"
	}
	return false
}

// smallBody runs read on the request body, which may be at most limit
// bytes and must arrive, all of it, within the body timeout.
//
// The time limit is a read deadline on the connection, and it is lifted
// again only once the whole body has been consumed:
//
//   - Lifted too early, and a client that stalls on the rest of its body
//     holds the connection for ever, because the HTTP server reads what is
//     left of a body before it answers.
//   - Left in place after the body has ended, and a slow response is cut
//     short: from then on the server keeps a read pending on the
//     connection to notice the client leaving, and that read hitting the
//     deadline cancels the request. A mirror's progress stream takes far
//     longer than this. (The server clears deadlines itself at the moment
//     a body ends, but a request with no body has already passed that
//     moment when the deadline is set here.)
func (s *Server) smallBody(w http.ResponseWriter, r *http.Request, limit int64, read func(body io.ReadCloser) error) error {
	rc := http.NewResponseController(w)
	timed := rc.SetReadDeadline(time.Now().Add(s.bodyTimeout())) == nil
	body := http.MaxBytesReader(baseWriter(w), r.Body, limit)
	err := read(body)
	_, rest := io.Copy(io.Discard, body)
	if rest == nil && timed {
		_ = rc.SetReadDeadline(time.Time{})
	}
	if err == nil {
		err = rest
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("the request body did not arrive within %s; send the request again", s.bodyTimeout())
	}
	return err
}

// readJSON decodes a small JSON request body into v. With strict, unknown
// fields are an error.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any, strict bool) error {
	return s.smallBody(w, r, limit, func(body io.ReadCloser) error {
		dec := json.NewDecoder(body)
		if strict {
			dec.DisallowUnknownFields()
		}
		return dec.Decode(v)
	})
}

// formValue reads one field of a small HTML form. A form that is too
// large, too slow or malformed reads as an empty value.
func (s *Server) formValue(w http.ResponseWriter, r *http.Request, key string) string {
	var value string
	_ = s.smallBody(w, r, maxFormBytes, func(body io.ReadCloser) error {
		r.Body = body
		value = r.FormValue(key)
		return nil
	})
	return value
}

// errTooLarge refuses content over a configured size limit. It is answered
// with 413.
type errTooLarge struct{ msg string }

func (e errTooLarge) Error() string { return e.msg }

// blobTooLarge reports whether size is over the operator's limit for one
// uploaded file, and the error to answer with if so.
func (s *Server) blobTooLarge(size int64) (bool, error) {
	limit := s.cfg.MaxBlobBytes
	if limit <= 0 || size <= limit {
		return false, nil
	}
	return true, s.blobLimitError()
}

func (s *Server) blobLimitError() error {
	return errTooLarge{fmt.Sprintf(
		"this file is larger than the %s this registry accepts in one file (REGISTRY_MAX_BLOB_BYTES is %d): ask the registry's operator to raise that setting",
		kit.HumanBytes(s.cfg.MaxBlobBytes), s.cfg.MaxBlobBytes)}
}
