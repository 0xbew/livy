// Package local is the directory sink: each object becomes a file at <root>/<key>. It is
// write-once like the bucket sinks: an existing file is never replaced, and "already
// there" is reported as fs.ErrExist.
package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Sink writes objects under Root.
type Sink struct{ Root string }

// New checks root is an absolute path; it is created on the first Put.
func New(root string) (*Sink, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("sink.path must be an absolute folder, got %q", root)
	}
	return &Sink{Root: filepath.Clean(root)}, nil
}

// URL is the file an object lands in.
func (s *Sink) URL(key string) string { return filepath.Join(s.Root, filepath.FromSlash(key)) }

// Put writes body to a fixed temp name beside the target and links it into place, so a
// reader never sees half an object and an object already there is never replaced.
func (s *Sink) Put(_ context.Context, key string, body []byte) error {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return fmt.Errorf("refusing object key %q", key)
	}
	final := s.URL(key)
	if err := os.MkdirAll(filepath.Dir(final), 0o777); err != nil {
		return err
	}
	if _, err := os.Lstat(final); err == nil {
		return s.existing(final)
	}
	// One fixed temp name, created exclusively: two emitters from different data folders
	// can share this sink, and a second writer must never truncate a temp file the first
	// is about to publish. A temp file left by a killed writer is cleared once it is stale.
	tmp := final + ".tmp"
	if st, err := os.Lstat(tmp); err == nil && time.Since(st.ModTime()) > staleTemp {
		_ = os.Remove(tmp)
	}
	if err := writeSynced(tmp, body); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			os.Remove(tmp)
		}
		// %v, not %w: a temp file that already exists is "not yet", and %w would make it fs.ErrExist, which the caller counts as shipped.
		return fmt.Errorf("%s: another writer is publishing this object (%v); retried next run", final, err)
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return s.existing(final)
		}
		return fmt.Errorf("%s: this folder cannot hard-link, so publishing cannot be create-only: %v", final, err)
	}
	// The new name, and any folder made for it (the root included), must outlive a power
	// cut before the caller deletes its copy.
	return syncDirs(filepath.Dir(final), filepath.Dir(s.Root))
}

// existing is the answer for an object already there. Its folders are synced first: a
// previous Put may have linked it and then failed to sync, and "already there" lets the
// caller delete its copy.
func (s *Sink) existing(final string) error {
	if err := syncDirs(filepath.Dir(final), filepath.Dir(s.Root)); err != nil {
		return err
	}
	return fmt.Errorf("%s: %w", final, fs.ErrExist)
}

// staleTemp is how old a temp file must be before it is taken for a killed writer's.
const staleTemp = 10 * time.Minute

// syncDirs fsyncs dir and each parent up to root.
func syncDirs(dir, root string) error {
	for {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return fmt.Errorf("syncing %s: %v", dir, err)
		}
		if dir == root || len(dir) <= len(root) {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

// writeSynced writes and fsyncs, so after a power cut an object that exists is whole: an
// existing object is taken as these bytes, and the local segment is then deleted.
func writeSynced(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Ready checks the root folder exists and is a folder, without writing anything. A root
// that does not exist yet is fine when its parent does: Put creates it.
func (s *Sink) Ready(_ context.Context) error {
	st, err := os.Stat(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		if _, perr := os.Stat(filepath.Dir(s.Root)); perr != nil {
			return fmt.Errorf("%s does not exist, nor its parent", s.Root)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a folder", s.Root)
	}
	return nil
}
