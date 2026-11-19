// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

// Package kit holds the few small helpers that more than one package of
// the registry needs: how a size and a revision id are shown, how a
// repository path is put in a URL, the two digests a file is known by, and
// writing a file so that it is never seen half-written.
//
// It imports only the standard library, so every package may use it,
// including internal/client, which shares nothing else with the server.
// Something belongs here only once two packages need it.
package kit

import (
	"crypto/sha1" //nolint:gosec // git blob ids are SHA-1 by definition; integrity rests on SHA-256.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"strings"
)

// HumanBytes writes a size the way people read one: "512 B", "1.5 KiB",
// "20.0 GiB". Units are powers of 1024.
func HumanBytes(n int64) string {
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

// ShortSHALen is how much of a revision id is shown where the whole of it
// would not fit.
const ShortSHALen = 12

// Short is the start of a revision id, for display.
func Short(sha string) string { return sha[:min(len(sha), ShortSHALen)] }

// EscapePath escapes a slash-separated path for use in a URL, one segment
// at a time, so the slashes stay and everything else is safe. It serves
// for a repository id ("namespace/name") and for a file's path in one.
func EscapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// SHA256Hex is the SHA-256 of v in lowercase hex: the name content is
// stored under.
func SHA256Hex[T ~string | ~[]byte](v T) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// GitBlobHasher returns a hash that yields the id git gives a file of size
// bytes, once exactly that many bytes of content are written to it. Git
// hashes a short header naming the size before the content, which is why
// the size has to be known first.
func GitBlobHasher(size int64) hash.Hash {
	h := sha1.New() //nolint:gosec // see the import.
	fmt.Fprintf(h, "blob %d\x00", size)
	return h
}

// GitBlobID is the id git gives content of size bytes, read from r.
func GitBlobID(r io.Reader, size int64) (string, error) {
	h := GitBlobHasher(size)
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// newFileMode is the mode of a file WriteNewFile creates: its owner only.
// What it writes is a copy of the metadata database.
const newFileMode = 0o600

// InstallFile copies src to the file at dst so that dst holds either what
// it held before or all of the new content, never part of it. The content
// goes into tmp, a file the caller has just opened for writing in dst's
// directory, which is flushed to disk, closed and renamed to dst. A
// failure removes tmp and leaves dst alone. It returns how many bytes were
// written.
//
// The caller opens tmp because how it is named is the caller's business: a
// random name where two writers may be at work, a fixed one where a
// leftover should be found and overwritten.
func InstallFile(tmp *os.File, dst string, src io.Reader) (int64, error) {
	n, err := fill(tmp, src, true)
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return n, nil
}

// WriteNewFile copies src to a new file at dst and refuses if anything is
// already there, so it can never overwrite. A failure part-way removes
// what was written. It returns how many bytes were written.
func WriteNewFile(dst string, src io.Reader) (int64, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, newFileMode) //nolint:gosec // G304: writing where the caller says is this function's purpose.
	if err != nil {
		return 0, err
	}
	n, err := fill(out, src, false)
	if err != nil {
		_ = os.Remove(dst)
		return 0, err
	}
	return n, nil
}

// fill copies src into out and closes it, flushing to disk first when
// asked. The file is closed whatever happens.
func fill(out *os.File, src io.Reader, sync bool) (int64, error) {
	n, err := io.Copy(out, src)
	if err == nil && sync {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return n, err
}
