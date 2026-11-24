package keyspace

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xbew/livy/internal/ledger"
)

// Kinds of lines a pending file holds, by its name's prefix.
const (
	kindLedger = "ledger"
	kindOtel   = "otel"
)

// writeWait bounds a writer's wait for the index lock. Writers hold it for well under a
// millisecond; past this a rebuild holds it, and the lines go to pending/ rather than keep a hook
// waiting.
const writeWait = 50 * time.Millisecond

// pendings numbers this process's pending files, so two in one nanosecond never share a name.
var pendings atomic.Int64

// AddLedger indexes ledger lines (whole lines, each ending in a newline) that were just
// appended to the ledger, its spill or a tool-decision file. It does nothing while the data
// folder has no index: a rebuild reads those lines from the ledger. Past the ledger's lock wait,
// or on a write error, the lines go to pending/ for readers to merge; an error means they could
// not be kept there either, and index/stale says so.
func AddLedger(dataDir string, data []byte) error {
	return add(dataDir, kindLedger, data, func() map[string][]byte { return ledgerLines(data) })
}

// AddOtel indexes otel stream lines (one export each) that were just appended to the stream,
// as AddLedger does ledger lines.
func AddOtel(dataDir string, data []byte) error {
	return add(dataDir, kindOtel, data, func() map[string][]byte {
		files, _ := otelLines(data) // an unreadable export is the stream's to report, not the index's
		return files
	})
}

func add(dataDir, kind string, data []byte, files func() map[string][]byte) error {
	idx := Dir(dataDir)
	// The lock file is the first thing a build makes: without it no index was ever started, and
	// the first build will read these lines from their source. With it, a line written while the
	// first build runs must reach pending/, or the build that read the ledger before it misses it.
	if !exists(filepath.Join(idx, lockName)) {
		return nil
	}
	out := files()
	if len(out) == 0 {
		return nil
	}
	release, err := ledger.Lock(filepath.Join(idx, lockName), writeWait)
	if err != nil {
		return keep(idx, kind, data, err)
	}
	werr := appendAll(idx, out)
	release()
	if werr != nil {
		return keep(idx, kind, data, werr)
	}
	return nil
}

// appendAll appends each file's lines to the live generation, under the caller's lock.
func appendAll(idx string, files map[string][]byte) error {
	gen, err := current(idx)
	if err != nil {
		return err
	}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if err := appendFile(genPath(idx, gen, rel), files[rel]); err != nil {
			return err
		}
	}
	return nil
}

// appendFile appends lines to path, after a newline when the file ends in a torn line.
func appendFile(path string, lines []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if _, err := ledger.AppendFile(path, lines); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// keep writes data to a new file in pending/, written whole under a temporary name and then
// renamed, so a reader never sees half of it. When even that fails, index/stale records why and
// readers fall back to the full scan until a rebuild.
func keep(idx, kind string, data []byte, why error) error {
	dir := filepath.Join(idx, pendingName)
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		name := fmt.Sprintf("%s-%d-%d-%d", kind, time.Now().UnixNano(), os.Getpid(), pendings.Add(1))
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, filepath.Join(dir, name+ext))
		}
		if err != nil {
			_ = os.Remove(tmp) // a temp name left behind is never read
		}
	}
	if err == nil {
		return nil
	}
	reason := fmt.Sprintf("%s: lines not indexed (%v) and not kept in pending (%v)\n", time.Now().UTC().Format(time.RFC3339), why, err)
	if serr := os.WriteFile(filepath.Join(idx, staleName), []byte(reason), 0o600); serr != nil {
		return fmt.Errorf("index: %w; and index/stale could not be written: %w", err, serr)
	}
	return fmt.Errorf("index: lines not kept, index marked stale: %w", err)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// current is the live generation's folder name.
func current(idx string) (string, error) {
	b, err := os.ReadFile(filepath.Join(idx, currentName))
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	gen := strings.TrimSpace(string(b))
	if !strings.HasPrefix(gen, genPrefix) || strings.ContainsAny(gen, `/\`) {
		return "", fmt.Errorf("index: CURRENT names %q, not a generation", gen)
	}
	return gen, nil
}

// pendingFiles is pending/'s files of one kind, oldest first by name.
func pendingFiles(idx, kind string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(idx, pendingName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), kind+"-") && strings.HasSuffix(e.Name(), ext) {
			out = append(out, filepath.Join(idx, pendingName, e.Name()))
		}
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(pendingOrder(a), pendingOrder(b)) })
	return out, nil
}

// pendingOrder sorts pending files by the time in their names, as written.
func pendingOrder(path string) string {
	parts := strings.SplitN(filepath.Base(path), "-", 3)
	if len(parts) < 3 {
		return path
	}
	n, _ := strconv.ParseInt(parts[1], 10, 64)
	return fmt.Sprintf("%020d", n) + parts[2]
}
