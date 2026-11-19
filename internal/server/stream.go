// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/model-delivery/registry/internal/blob"
	"github.com/model-delivery/registry/internal/registry"
)

// Serve while fetching: a file that pull-through is still downloading is
// sent to the clients waiting for it as it arrives, so the first download
// of a large file takes about as long as the fetch and the first byte
// reaches the client at once. Without it a client receives nothing until
// the whole file is stored, and download libraries give up long before
// that (huggingface_hub waits ten seconds).
//
// It is the one place where bytes leave the registry before they are
// verified, and these are the rules that keep that from ever becoming a
// complete file that was not verified:
//
//   - Every such response runs to the end of the file, and the end is held
//     back: the last streamHoldBack bytes are sent only after the digest
//     and the size have been checked. If the check fails, or the fetch
//     fails for any other reason, the connection is aborted. The client was
//     promised Content-Length bytes and got fewer.
//   - A file of streamHoldBack bytes or less is all end. Nothing of it is
//     sent before it is verified and stored, and it is served as before.
//   - Nothing about storing changes. The bytes are staged, verified and
//     published by blob.Store exactly as without streaming, and a file's
//     row moves to `files` only afterwards.
//   - A client keeps what it received and may ask for the rest ("Range:
//     bytes=N-"). While the same fetch is in flight that is safe, and is
//     served from it: what the client holds and what it is sent are one
//     download, and its end is still held back. After a fetch has failed it
//     is not: the rest, taken from a later and sound fetch, would complete
//     a file whose start was never verified. So a file whose streamed fetch
//     failed is distrusted (puller.distrust): it is fetched, verified and
//     stored before anything of it is sent again, and a request for part of
//     it is answered with all of it (200, not 206), which makes a client
//     drop what it has. huggingface_hub does exactly that (http_get in its
//     file_download.py truncates its temporary file when a ranged request
//     is answered 200); test/e2e runs it.
//
// The distrust is kept in memory. docs/operations.md says what that leaves
// open: a client that is not huggingface_hub and mishandles a 200, and a
// registry restarted between the failure and the client's retry.

// streamHoldBack is how much of a file's end is not sent until the file
// has been verified.
//
// One byte would be enough for a client that compares what it received
// with Content-Length, and huggingface_hub does. It is a mebibyte because
// not every client does: one that keeps whatever arrived must be left with
// a file its loader refuses, and model formats differ in how much of the
// end they need. A zip archive (PyTorch checkpoints, .npz) keeps its
// directory at the end; a mebibyte covers the end-of-archive record and its
// comment, at most 64 KiB, and the directory of any ordinary checkpoint.
// safetensors and GGUF loaders check the length the header declares, so any
// missing end fails them.
//
// It also sets which files are streamed at all: one that fits in the
// hold-back would have nothing sent early, so it is simply served after it
// is stored, with a status that says what went wrong if something did.
// Configuration and tokenizer files are all of that kind, and a mebibyte
// arrives from the upstream well inside a client's timeout.
const streamHoldBack = 1 << 20

// streamChunk is the most that is read from the staging file and written
// to a client in one piece.
const streamChunk = 256 << 10

// streamFrom reports whether a request for the pending file f may be
// answered from the fetch in flight, and from which byte.
//
// It may when streaming is on, the file is larger than the hold-back and
// is not distrusted, and the request asks for the file from some byte to
// its end with no condition attached. Everything else (a bounded range, a
// suffix, several ranges, If-None-Match and its kin) waits for the stored
// file, where http.ServeContent answers it in full.
func (s *Server) streamFrom(r *http.Request, f registry.File) (start int64, ok bool) {
	if !s.puller.stream || f.Size <= streamHoldBack || s.puller.distrusts(f.ETag()) {
		return 0, false
	}
	for _, conditional := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if r.Header.Get(conditional) != "" {
			return 0, false
		}
	}
	ranges := r.Header.Values("Range")
	if len(ranges) == 0 {
		return 0, true
	}
	if len(ranges) > 1 {
		return 0, false
	}
	// Only "bytes=N-": the rest of the file from N.
	first, found := strings.CutPrefix(ranges[0], "bytes=")
	first, open := strings.CutSuffix(first, "-")
	if !found || !open || first == "" || strings.Trim(first, "0123456789") != "" {
		return 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start >= f.Size {
		return 0, false
	}
	return start, true
}

// serveWhileFetching answers a request for f, from byte start to the end,
// out of the fetch job has in flight. It reports false, having written
// nothing, when that fetch ended before there was a byte to send: the
// caller then answers from the job's result, with a status if it failed.
//
// Once the first byte is sent the answer cannot be changed, so a fetch
// that fails after that ends the response the only way left, by aborting
// the connection, and says why in the log.
func (s *Server) serveWhileFetching(w http.ResponseWriter, r *http.Request, job *pullJob[struct{}], c registry.Commit, f registry.File, start int64) bool {
	follower := job.tail.Follow()
	if follower == nil {
		return false // the fetch has just ended
	}
	defer follower.Close()
	ctx := r.Context()
	// Bytes before held may leave before the file is verified; bytes from
	// there on may not.
	held := f.Size - streamHoldBack
	received, verified, err := follower.Await(ctx, start, start >= held)
	if err != nil {
		// Nothing was received, the client left, or the content was already
		// stored and never downloaded: nothing is sent from here.
		return false
	}

	// The headers of a stored file, since a client decides what to do from
	// them. ETag, Cache-Control and X-Repo-Commit are set by the caller.
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", path.Base(f.Path)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(f.Size-start, 10))
	if s.cfg.RedirectDownloads && s.reg.Presigns() {
		// Nothing is in the object store to redirect to yet. These are how a
		// redirected answer names the file, and later answers will carry them.
		h.Set("X-Linked-Etag", `"`+f.ETag()+`"`)
		h.Set("X-Linked-Size", strconv.FormatInt(f.Size, 10))
	}
	status := http.StatusOK
	if start > 0 {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, f.Size-1, f.Size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	s.m.pull.streamsStarted.Inc()

	flusher := http.NewResponseController(w)
	buf := make([]byte, streamChunk)
	off := start
	for off < f.Size {
		limit := min(received, f.Size)
		if !verified {
			limit = min(limit, held)
		}
		if off >= limit {
			// Flushed before every wait, so the client has all there is while
			// the upstream is slow. Nothing is sent to keep it waiting: an
			// upstream that stalls stalls the client.
			_ = flusher.Flush()
			if received, verified, err = follower.Await(ctx, off, off >= held); err != nil {
				if ctx.Err() != nil {
					return true // the client left; the fetch carries on for the others
				}
				s.abortStream(r, job, c, f, off-start, err)
			}
			continue
		}
		n, err := follower.ReadAt(buf[:min(int64(len(buf)), limit-off)], off)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true // the client left
			}
			off += int64(n)
			s.m.bytesFromFetch.Add(int64(n)) // once for each piece of up to streamChunk
		}
		if err != nil && n == 0 {
			s.abortStream(r, job, c, f, off-start, err)
		}
	}
	return true
}

// abortStream ends a response that was being sent from a fetch in flight,
// after sent bytes of it, because the fetch failed or its staging file
// could not be read. It does not return: the connection is closed without
// the response being completed, so the client sees a transfer cut short of
// the length it was given.
func (s *Server) abortStream(r *http.Request, job *pullJob[struct{}], c registry.Commit, f registry.File, sent int64, cause error) {
	s.m.pull.streamsAborted.Inc()
	if errors.Is(cause, blob.ErrTailEnded) {
		// The fetch's own error is the reason. It is set by the time the tail
		// ends, and readable once the job is done, which is a moment later.
		<-job.done
		cause = job.err
		if cause == nil {
			cause = errors.New("the fetch finished, and this response had not been sent the bytes while they could be read")
		}
	}
	level, reason := s.log.Error, "the registry could not finish fetching the file"
	switch {
	case errors.Is(cause, errShuttingDown), errors.Is(cause, context.Canceled):
		level, reason = s.log.Warn, "the registry is shutting down"
	case isUpstreams(cause):
		level, reason = s.log.Warn, "the upstream's content could not be fetched or did not match its digest"
	}
	level("aborted a download that was being sent while the file was fetched: "+reason,
		"repo", c.Key(), "sha", c.SHA, "path", f.Path, "sent", sent, "size", f.Size, "url", r.URL.Path, "error", cause)
	panic(http.ErrAbortHandler)
}

// isUpstreams reports whether a fetch's failure was the upstream's doing.
func isUpstreams(err error) bool {
	return errors.As(pullFailure(err), new(errUpstream))
}
