package keyspace

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/otelspan"
)

// RebuildWait is how long a rebuild waits for writers to let go of the index lock.
const RebuildWait = 10 * time.Second

// coarseClock is the coarsest file time a filesystem keeps.
const coarseClock = 2 * time.Second

// flushAt is how many bytes a rebuild holds in memory before writing them out.
const flushAt = 32 << 20

// Manifest is a generation's built.json: what built it and what it holds.
type Manifest struct {
	Schema  string    `json:"schema"`
	BuiltAt time.Time `json:"built_at"`
	Livy    string    `json:"livy"`
	Lines   int       `json:"ledger_lines"`
	Spans   int       `json:"spans"`
	Files   int       `json:"files"`
	// Bad counts the source lines a rebuild could not read.
	Bad int `json:"unreadable_lines"`
	// Unread are the sources a rebuild could not read (a file, a folder): readers of this
	// generation report them, as the full scan reports them every time it runs.
	Unread []string `json:"unread"`
}

// Builder writes one new generation from the sources a rebuild feeds it, each ledger line and
// each span once, in the order fed: the order `livy why` and `livy profile` read them.
type Builder struct {
	dir      string
	files    map[string][]byte
	held     int
	seen     map[[sha256.Size]byte]bool
	spanSeen map[[2]string]bool
	written  map[string]bool
	m        Manifest
	// err is the first write that failed: the build is then refused, not made live short.
	err error
}

// Ledger adds one ledger line (without its newline).
func (b *Builder) Ledger(line []byte) error {
	sum := sha256.Sum256(line)
	if b.seen[sum] {
		return nil
	}
	b.seen[sum] = true
	k, ok := KeysOf(line)
	if !ok {
		b.m.Bad++
		return nil
	}
	b.m.Lines++
	for _, f := range ledgerFiles(k) {
		b.hold(f, append(append([]byte(nil), line...), '\n'))
	}
	return b.flushIfFull()
}

// Otel adds one otel stream line (one export); a line of another signal adds nothing.
func (b *Builder) Otel(line []byte) error {
	spans, err := otelspan.Decode(line)
	if err != nil {
		b.m.Bad++
		return nil // counted, as profile counts it, and the rest is read
	}
	fresh := spans[:0]
	for _, s := range spans {
		k := [2]string{s.Trace, s.ID}
		if !b.spanSeen[k] {
			b.spanSeen[k] = true
			fresh = append(fresh, s)
		}
	}
	files, err := spanFiles(fresh)
	if err != nil {
		return err
	}
	b.m.Spans += len(fresh)
	for f, data := range files {
		b.hold(f, data)
	}
	return b.flushIfFull()
}

// BadLine counts a source line the feed could not read.
func (b *Builder) BadLine() { b.m.Bad++ }

// Unreadable records a source (a file or a folder) the feed could not read, and why.
func (b *Builder) Unreadable(what string) { b.m.Unread = append(b.m.Unread, what) }

func (b *Builder) hold(rel string, data []byte) {
	b.files[rel] = append(b.files[rel], data...)
	b.held += len(data)
}

func (b *Builder) flushIfFull() error {
	if b.held < flushAt || b.err != nil {
		return b.err
	}
	b.err = b.flush()
	return b.err
}

func (b *Builder) flush() error {
	for rel, data := range b.files {
		if err := appendFile(filepath.Join(b.dir, rel), data); err != nil {
			return err
		}
		b.written[rel] = true
	}
	b.files, b.held = map[string][]byte{}, 0
	return nil
}

// Rebuild builds a new generation of dataDir's index from what feed adds, holding the index
// lock throughout (writers meanwhile keep their lines in pending/), then makes it the live one
// and deletes the older generations, the stale mark, and the pending files that were there
// before it began: their lines were in the sources it read.
func Rebuild(dataDir, version string, feed func(*Builder) error) (Manifest, error) {
	idx := Dir(dataDir)
	if err := os.MkdirAll(idx, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("index: %w", err)
	}
	release, err := ledger.Lock(filepath.Join(idx, lockName), RebuildWait)
	if err != nil {
		return Manifest{}, fmt.Errorf("index lock: %w", err)
	}
	defer release()
	before, err := allPending(idx)
	if err != nil {
		return Manifest{}, err
	}
	started := time.Now()
	gen := genPrefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	b := &Builder{dir: filepath.Join(idx, gen), files: map[string][]byte{}, seen: map[[sha256.Size]byte]bool{},
		spanSeen: map[[2]string]bool{}, written: map[string]bool{},
		m: Manifest{Schema: Schema, BuiltAt: time.Now().UTC(), Livy: version, Unread: []string{}}}
	if err := build(b, feed); err != nil {
		_ = os.RemoveAll(b.dir) // never made live: the failure is what is reported
		return Manifest{}, err
	}
	if err := swap(idx, gen); err != nil {
		_ = os.RemoveAll(b.dir) // never made live: the failure is what is reported
		return Manifest{}, err
	}
	return b.m, tidy(idx, gen, before, started)
}

// build feeds b, writes out what it holds and its manifest.
func build(b *Builder, feed func(*Builder) error) error {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if err := feed(b); err != nil {
		return err
	}
	if b.err != nil {
		return b.err
	}
	if err := b.flush(); err != nil {
		return err
	}
	b.m.Files = len(b.written)
	mb, err := json.MarshalIndent(b.m, "", "  ")
	if err != nil {
		return fmt.Errorf("index manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(b.dir, manifestName), append(mb, '\n'), 0o600); err != nil {
		return fmt.Errorf("index manifest: %w", err)
	}
	return nil
}

// swap makes gen the live generation, by replacing CURRENT whole.
func swap(idx, gen string) error {
	tmp := filepath.Join(idx, "."+currentName+".tmp")
	if err := os.WriteFile(tmp, []byte(gen+"\n"), 0o600); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(idx, currentName)); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// tidy deletes every generation but gen, the pending files in before, and the stale mark when it
// is older than the build: one written during it may name a line the build did not read.
func tidy(idx, gen string, before []string, started time.Time) error {
	var errs []error
	entries, err := os.ReadDir(idx)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), genPrefix) && e.Name() != gen {
			errs = append(errs, os.RemoveAll(filepath.Join(idx, e.Name())))
		}
	}
	stale := filepath.Join(idx, staleName)
	// Clearly older: a coarse clock (HFS+, FAT) can date a mark written during the build before it.
	if fi, err := os.Stat(stale); err == nil && fi.ModTime().Before(started.Add(-coarseClock)) {
		before = append(before, stale)
	}
	for _, p := range before {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("index: the new generation is live, but tidying up failed: %w", err)
	}
	return nil
}

// allPending is every pending file, of either kind.
func allPending(idx string) ([]string, error) {
	l, err := pendingFiles(idx, kindLedger)
	if err != nil {
		return nil, err
	}
	o, err := pendingFiles(idx, kindOtel)
	return append(l, o...), err
}
