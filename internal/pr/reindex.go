package pr

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/buildinfo"
	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/keyspace"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/ledgerfiles"
	"github.com/0xbew/livy/internal/otelspan"
	"github.com/0xbew/livy/internal/relay"
)

// errOpen marks a source file that exists but could not be opened: the index is built without it.
var errOpen = errors.New("could not open")

// maxLedgerLine is the longest ledger line a rebuild reads, as `livy why`.
const maxLedgerLine = 16 << 20

// ReindexMain runs `livy reindex`: rebuild the keyspace index from every line on the machine.
// 0 when it was rebuilt, 1 when it was rebuilt but some files could not be read, 2 when it could not be.
func ReindexMain(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: livy reindex")
		return 2
	}
	m, unread, err := Reindex(dataDir())
	if err != nil {
		fmt.Fprintf(stderr, "livy reindex: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "index rebuilt: %d ledger lines and %d spans in %d files; %d unreadable lines\n",
		m.Lines, m.Spans, m.Files, m.Bad)
	for _, u := range unread {
		fmt.Fprintf(stdout, "%s could not be read\n", u)
	}
	if len(unread) > 0 {
		return 1
	}
	return 0
}

// Reindex rebuilds dataDir's index from the sources `livy why` and `livy profile` read, in
// their order, and names the folders that exist but could not be read.
func Reindex(dataDir string) (keyspace.Manifest, []string, error) {
	version := "(no build info)"
	if b, ok := buildinfo.Read(); ok {
		version = b.Rev
	}
	var unread []string
	m, err := keyspace.Rebuild(dataDir, version, func(b *keyspace.Builder) error {
		unread = feed(b, dataDir)
		for _, u := range unread {
			b.Unreadable(u)
		}
		return nil
	})
	return m, unread, err
}

// feed adds every ledger line, then every otel stream line, to b.
func feed(b *keyspace.Builder, dataDir string) []string {
	var unread []string
	file := func(add func([]byte) error) func(string) {
		return func(p string) {
			switch err := eachLine(p, add); {
			case err == nil, errors.Is(err, fs.ErrNotExist):
				// read, or folded away (a spill, a pending file) since the folder was listed
			case errors.Is(err, errOpen):
				unread = append(unread, err.Error())
			default:
				b.BadLine() // as the scan counts a file it opened but could not read through
			}
		}
	}
	ledgerLine := file(b.Ledger)
	if err := eachLine(filepath.Join(dataDir, ledger.FileName), b.Ledger); err != nil && !errors.Is(err, fs.ErrNotExist) {
		unread = append(unread, "ledger")
	}
	sink := ledgerfiles.LocalSink()
	for _, root := range []string{filepath.Join(dataDir, ledger.SpillDir), filepath.Join(dataDir, ledger.SegmentsDir),
		filepath.Join(dataDir, ledger.UploadedDir), filepath.Join(dataDir, relay.DecisionsDir)} {
		unread = append(unread, walkRoot(root, ledgerLine)...)
	}
	if sink != "" {
		unread = append(unread, walkRoot(sink, ledgerLine)...)
	}
	otelLine := file(b.Otel)
	unread = append(unread, walkRoot(relay.OtelStreamDir(dataDir), otelLine)...)
	if sink != "" {
		unread = append(unread, walkRoot(sink, otelLine)...)
	}
	return unread
}

// walkRoot reads every ledger file under root; a root that is not there has none.
func walkRoot(root string, f func(string)) []string {
	n, err := ledgerfiles.Walk(root, f)
	var out []string
	if n > 0 {
		out = append(out, fmt.Sprintf("%d folders under %s", n, root))
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		out = append(out, root)
	}
	return out
}

// eachLine calls add on each line of path, gzipped when its name ends in .gz.
func eachLine(path string, add func([]byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w %s: %w", errOpen, path, err)
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	var r io.Reader = fh
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return fmt.Errorf("gunzip %s: %w", path, err)
		}
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), max(maxLedgerLine, otelspan.MaxLine))
	for sc.Scan() {
		if err := add(sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// Daily keeps the index fit for `livy pr`, for the daily job: rebuilt when it is missing, stale or
// of another layout, else pruned of files untouched for longer than kept segments are kept.
// What it could not do goes to stderr and a ledger.error; it never stops the job.
func Daily(stderr io.Writer) {
	dir := dataDir()
	var unusable *keyspace.Unusable
	_, err := keyspace.Open(dir)
	switch {
	case errors.As(err, &unusable):
		if _, unread, err := Reindex(dir); err != nil {
			hook.WriteError("reindex: "+err.Error(), stderr)
		} else if len(unread) > 0 {
			hook.WriteError("reindex: could not read "+strings.Join(unread, ", "), stderr)
		}
	case err != nil:
		hook.WriteError("reindex: "+err.Error(), stderr)
	default:
		keep, _ := emit.KeepUploaded()
		if err := keyspace.Prune(dir, keep); err != nil {
			hook.WriteError("index prune: "+err.Error(), stderr)
		}
	}
}
