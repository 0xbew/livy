package keyspace

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxLine is the longest index line read: a span line is small, a ledger line at most the
// ledger's own longest.
const maxLine = 16 << 20

// Index is one generation of a data folder's index, opened to read.
type Index struct {
	idx, gen string
	Manifest Manifest
}

// Unusable is why there is no index to read: none was built, a write could not be kept, or it
// was built under another layout. Readers then scan the sources, as before the index.
type Unusable struct{ Reason string }

func (u *Unusable) Error() string { return u.Reason }

// Open is dataDir's live generation. An *Unusable error says why there is none to read.
func Open(dataDir string) (*Index, error) {
	idx := Dir(dataDir)
	if b, err := os.ReadFile(filepath.Join(idx, staleName)); err == nil {
		return nil, &Unusable{Reason: "the index is stale (" + strings.TrimSpace(string(b)) + "); run `livy reindex`"}
	}
	if !exists(filepath.Join(idx, currentName)) {
		return nil, &Unusable{Reason: "no index has been built; run `livy reindex`"}
	}
	gen, err := current(idx)
	if err != nil {
		return nil, &Unusable{Reason: err.Error()}
	}
	mb, err := os.ReadFile(filepath.Join(idx, gen, manifestName))
	if err != nil {
		return nil, &Unusable{Reason: "the index has no readable built.json; run `livy reindex`"}
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.Schema != Schema {
		return nil, &Unusable{Reason: fmt.Sprintf("the index was built as %q, this livy reads %s; run `livy reindex`", m.Schema, Schema)}
	}
	return &Index{idx: idx, gen: gen, Manifest: m}, nil
}

// Path is rel's file in this generation.
func (x *Index) Path(rel string) string { return genPath(x.idx, x.gen, rel) }

// Changed says whether a rebuild made another generation live since Open: what was read may
// mix the two, so the reader reads again.
func (x *Index) Changed() bool {
	gen, err := current(x.idx)
	return err != nil || gen != x.gen
}

// Lines is rel's lines, without newlines; none when the file does not exist.
func (x *Index) Lines(rel string) ([][]byte, error) {
	return ReadLines(x.Path(rel))
}

// PendingLedger and PendingOtel are the pending files of each kind, which readers merge.
func PendingLedger(dataDir string) ([]string, error) { return pendingFiles(Dir(dataDir), kindLedger) }

// PendingOtel is the pending files of otel stream lines.
func PendingOtel(dataDir string) ([]string, error) { return pendingFiles(Dir(dataDir), kindOtel) }

// ReadLines is an uncompressed JSON-lines file's non-empty lines, without newlines; none when the
// file does not exist. Readers use it for the live ledger and pending files as well as the index's.
func ReadLines(path string) ([][]byte, error) {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	var out [][]byte
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("index: read %s: %w", path, err)
	}
	return out, nil
}
