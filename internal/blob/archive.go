// Copyright 2026 Spectrum Labs LLC
// SPDX-License-Identifier: Apache-2.0

package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// Archive keeps metadata backups next to the blobs: in the data directory
// for the filesystem store, in the same bucket for S3. Keeping them with
// the content they describe means one place to protect, and with object
// storage it puts them off the server's disk.
//
// Archives are small named files, unlike blobs, which are named by digest.
// Every Store is an Archive.
type Archive interface {
	PutArchive(ctx context.Context, name, localPath string) error
	ListArchives(ctx context.Context) ([]ArchiveInfo, error)
	OpenArchive(ctx context.Context, name string) (io.ReadCloser, error)
	DeleteArchive(ctx context.Context, name string) error
}

// ArchiveInfo describes one stored backup.
type ArchiveInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"created_at"`
}

// ErrArchiveNotFound reports that no backup has the given name.
var ErrArchiveNotFound = errors.New("backup not found")

var archiveNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,120}\.db$`)

// validArchiveName keeps archive names to plain file names, so one can
// never address anything outside the backups area. The pattern already
// rules out a path separator; "..", which it would allow inside a name, is
// refused as well so that no name even resembles a path.
func validArchiveName(name string) error {
	if !archiveNameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w: %q is not a backup name", ErrArchiveNotFound, name)
	}
	return nil
}

func sortArchives(list []ArchiveInfo) {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
}

// ---- filesystem ----

func (s *FS) archivePath(name string) string { return filepath.Join(s.root.Backups(), name) }

// PutArchive copies localPath into the backups directory under name. The
// copy appears under its final name only once it is complete.
func (s *FS) PutArchive(_ context.Context, name, localPath string) error {
	if err := validArchiveName(name); err != nil {
		return err
	}
	if err := s.putArchive(name, localPath); err != nil {
		return fmt.Errorf("fs: store backup %s: %w", name, err)
	}
	return nil
}

func (s *FS) putArchive(name, localPath string) error {
	if err := os.MkdirAll(s.root.Backups(), 0o750); err != nil {
		return err
	}
	src, err := os.Open(localPath) //nolint:gosec // a snapshot this process just wrote.
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(s.root.Backups(), ".incoming-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.archivePath(name))
}

// ListArchives lists stored backups, oldest first.
func (s *FS) ListArchives(context.Context) ([]ArchiveInfo, error) {
	entries, err := os.ReadDir(s.root.Backups())
	if errors.Is(err, os.ErrNotExist) {
		return []ArchiveInfo{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fs: list backups: %w", err)
	}
	return archiveInfos(entries)
}

// archiveInfos describes the backups among a directory's entries. A backup
// that was removed since the directory was read is passed over; one that
// is there and cannot be examined is an error, not a shorter list, because
// what the list is used for is deciding which backups to delete.
func archiveInfos(entries []fs.DirEntry) ([]ArchiveInfo, error) {
	out := []ArchiveInfo{}
	for _, e := range entries {
		if e.IsDir() || validArchiveName(e.Name()) != nil {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fs: list backups: examine %s: %w", e.Name(), err)
		}
		out = append(out, ArchiveInfo{Name: e.Name(), Size: fi.Size(), ModTime: fi.ModTime().UTC()})
	}
	sortArchives(out)
	return out, nil
}

// OpenArchive opens a stored backup.
func (s *FS) OpenArchive(_ context.Context, name string) (io.ReadCloser, error) {
	if err := validArchiveName(name); err != nil {
		return nil, err
	}
	f, err := os.Open(s.archivePath(name)) //nolint:gosec // name was validated above.
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrArchiveNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("fs: open backup %s: %w", name, err)
	}
	return f, nil
}

// DeleteArchive removes a stored backup. Removing one that is not there is
// not an error.
func (s *FS) DeleteArchive(_ context.Context, name string) error {
	if err := validArchiveName(name); err != nil {
		return err
	}
	if err := os.Remove(s.archivePath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fs: delete backup %s: %w", name, err)
	}
	return nil
}

// ---- S3 ----

func (s *S3) archiveKey(name string) string { return s.prefix + "backups/" + name }

// PutArchive uploads localPath to the bucket under backups/name.
func (s *S3) PutArchive(ctx context.Context, name, localPath string) error {
	if err := validArchiveName(name); err != nil {
		return err
	}
	f, err := os.Open(localPath) //nolint:gosec // a snapshot this process just wrote.
	if err != nil {
		return fmt.Errorf("s3: upload backup %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("s3: upload backup %s: %w", name, err)
	}
	if _, err := s.client.PutObject(ctx, s.bucket, s.archiveKey(name), f, fi.Size(),
		minio.PutObjectOptions{ContentType: "application/vnd.sqlite3"}); err != nil {
		return fmt.Errorf("s3: upload backup %s: %w", name, err)
	}
	return nil
}

// ListArchives lists stored backups, oldest first.
func (s *S3) ListArchives(ctx context.Context) ([]ArchiveInfo, error) {
	prefix := s.archiveKey("")
	out := []ArchiveInfo{}
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("s3: list backups: %w", obj.Err)
		}
		name := strings.TrimPrefix(obj.Key, prefix)
		if validArchiveName(name) != nil {
			continue
		}
		out = append(out, ArchiveInfo{Name: name, Size: obj.Size, ModTime: obj.LastModified.UTC()})
	}
	sortArchives(out)
	return out, nil
}

// OpenArchive opens a stored backup.
func (s *S3) OpenArchive(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := validArchiveName(name); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, s.archiveKey(name), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3: open backup %s: %w", name, err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if isNoSuchKey(err) {
			return nil, fmt.Errorf("%w: %s", ErrArchiveNotFound, name)
		}
		return nil, fmt.Errorf("s3: open backup %s: %w", name, err)
	}
	return obj, nil
}

// DeleteArchive removes a stored backup.
func (s *S3) DeleteArchive(ctx context.Context, name string) error {
	if err := validArchiveName(name); err != nil {
		return err
	}
	if err := s.client.RemoveObject(ctx, s.bucket, s.archiveKey(name), minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return fmt.Errorf("s3: delete backup %s: %w", name, err)
	}
	return nil
}
