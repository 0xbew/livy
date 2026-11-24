package ledger

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Tail is a JSON-lines file opened to append to, with what a check of its end found: a write
// killed partway leaves its last line unfinished, and an append glued to that fragment is lost
// with it.
type Tail struct {
	File *os.File
	// Before is the file as it was opened, to cut a failed write back to; nil when it could not
	// be read.
	Before fs.FileInfo
	// Unchecked is why the end could not be checked for a torn write. The append then goes ahead
	// as it did before the check existed, with no newline in front.
	Unchecked error
	torn      bool
}

// OpenTail opens path to append to, creating it 0600, read-write so its last byte can be
// checked. A file that can be written but not read is opened write-only, with why in
// Unchecked. An error means it could not be opened at all.
func OpenTail(path string) (*Tail, error) {
	var unchecked error
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		unchecked = withoutPath(err)
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	t := &Tail{File: f}
	before, err := f.Stat()
	switch {
	case err != nil:
		t.Unchecked = withoutPath(err)
	case unchecked != nil:
		t.Before, t.Unchecked = before, unchecked
	default:
		t.Before = before
		t.torn, t.Unchecked = endsTorn(f, before.Size())
	}
	return t, nil
}

// Lead is data with a newline in front when the file ends in a torn line, so the append's first
// line starts on a line of its own and only the fragment is lost.
func (t *Tail) Lead(data []byte) []byte {
	if !t.torn {
		return data
	}
	return append([]byte{'\n'}, data...)
}

// CutBack truncates the file to its size when opened, after a write that failed partway, so the
// next line never lands glued to half of this one. Best effort: the write's error is what is
// reported. Safe only while every writer of the file holds the same lock.
func (t *Tail) CutBack() {
	if t.Before != nil {
		_ = t.File.Truncate(t.Before.Size())
	}
}

// AppendFile appends data (whole lines) to the JSON-lines file at path, created 0600, after a
// newline when the file ends in a torn line; a write that fails partway is cut back. unchecked
// is why the end could not be checked; the lines were then written without the check. The
// caller holds whatever lock guards path.
func AppendFile(path string, data []byte) (unchecked, err error) {
	t, err := OpenTail(path)
	if err != nil {
		return nil, err
	}
	if _, err := t.File.Write(t.Lead(data)); err != nil {
		t.CutBack()
		_ = t.File.Close() // the failed write is what is reported
		return nil, fmt.Errorf("ledger: %w", err)
	}
	if err := t.File.Close(); err != nil {
		return t.Unchecked, fmt.Errorf("ledger: %w", err)
	}
	return t.Unchecked, nil
}

// endsTorn is whether f, of size bytes, ends without a newline, or why its last byte could not
// be read.
func endsTorn(f *os.File, size int64) (bool, error) {
	if size == 0 {
		return false, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return false, withoutPath(err)
	}
	return last[0] != '\n', nil
}

// withoutPath is err without the file name an *fs.PathError carries: the reason goes into a
// ledger.error, which names no local path.
func withoutPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
