// Package ledger appends lines to the local ledger, under the lock the emitter's rotation
// shares. A line is never written to ledger.jsonl without that lock: when the lock cannot
// be had in time, the line goes to its own file in spill/, which the emitter folds in.
package ledger

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

// ErrClose is a close that failed after a full write: the line may or may not be on disk,
// so the caller says so rather than writing it again.
var ErrClose = errors.New("ledger: close failed after the write")

// write is the ledger's one write; a seam so a test can fail it partway, as a full disk does.
var write = (*os.File).Write

// spills numbers this process's spill files, so two in one nanosecond never share a name.
var spills atomic.Int64

// File and directory names inside the data directory.
const (
	FileName = "ledger.jsonl"
	LockName = "ledger.lock"
	SpillDir = "spill"
)

// The folders emit keeps ledger segments in: rotated and not yet shipped, and a copy of each the
// sink has. emit owns them; they are named here so a reader emit depends on can find them.
const (
	SegmentsDir = "segments"
	UploadedDir = "uploaded"
)

// DataNames are the files and folders this package keeps in the data folder, by exact name.
// `livy uninstall --purge` deletes these and nothing else.
func DataNames() []string { return []string{FileName, LockName, SpillDir} }

// LockWait bounds the wait for the rotation lock, which rotation holds only across a
// rename; past it the line is spilled, so a hung holder never blocks a hook.
var (
	LockWait = 200 * time.Millisecond
	lockPoll = 5 * time.Millisecond
)

// Appended is what an Append that wrote its lines did.
type Appended struct {
	Spilled bool // the lines went to a new file in spill/, not to the ledger
	// Unchecked is why the ledger's last line could not be checked for a torn write; the lines
	// were then written as they were before the check existed, with no newline in front.
	Unchecked error
}

// Append writes data (whole lines, each ending in a newline) to dir's ledger in one write
// under the lock, or, when the lock cannot be had within LockWait, to a new file in
// spill/. A ledger that ends in a torn line (a writer killed mid-write) gets a newline first,
// so the first line is never glued to the fragment. An error means the line was written nowhere.
func Append(dir string, data []byte) (Appended, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Appended{}, fmt.Errorf("ledger: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, LockName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Appended{Spilled: true}, spill(dir, data) // a lock that cannot be opened is a lock not acquired
	}
	defer lock.Close()
	if !acquire(lock, LockWait) {
		return Appended{Spilled: true}, spill(dir, data)
	}
	defer unlock(lock)
	// A write that fails partway (a full disk) is cut back to where it started. Safe only
	// because every writer of this file holds the lock: Livy's never writes unlocked, and the
	// Python tool must not share a data dir with it (the dogfood week gives Go its own).
	t, err := OpenTail(filepath.Join(dir, FileName))
	if err != nil {
		return Appended{}, err
	}
	if _, err := write(t.File, t.Lead(data)); err != nil {
		t.CutBack()
		_ = t.File.Close() // the failed write is what is reported
		return Appended{}, err
	}
	if err := t.File.Close(); err != nil {
		// The bytes were written; a late failure (NFS, quota) may still have lost them.
		return Appended{Unchecked: t.Unchecked}, fmt.Errorf("%w: %w", ErrClose, err)
	}
	return Appended{Unchecked: t.Unchecked}, nil
}

func acquire(f *os.File, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if tryLock(f) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(lockPoll)
	}
}

// spill writes the lines to a temp name and links it into place, so a reader that lists
// spill/*.jsonl never sees a half-written file. A temp name left by a killed hook is the
// emitter's to sweep; a hook never sweeps, since another hook's temp file may be live.
func spill(dir string, data []byte) error {
	sd := filepath.Join(dir, SpillDir)
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s-%d", time.Now().UnixNano(), strconv.Itoa(os.Getpid()), spills.Add(1))
	tmp := filepath.Join(sd, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	// Link, not rename, so publishing never replaces a spill file; a filesystem without
	// hard links (FAT, some network mounts) falls back to rename, as names are unique.
	final := filepath.Join(sd, name+".jsonl")
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, final)
	}
	_ = os.Remove(tmp) // published; a temp name left behind is the emitter's to sweep
	return nil
}
