// Package ledgerfiles reads the ledger lines this machine keeps: the ledger, its spill, emit's
// segments (unshipped, and the copies kept after upload) and a local sink's folder.
package ledgerfiles

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
)

// maxLine is the longest ledger line read.
const maxLine = 16 << 20

const eventRecorded = "commit.recorded"

// LocalSink is the folder a local sink ships to, "" for any other sink.
func LocalSink() string {
	s := hook.Settings().Settings
	if t, _ := s["sink.type"].Value.(string); t != "local" {
		return ""
	}
	p, _ := s["sink.path"].Value.(string)
	return p
}

// Walk calls f on every *.jsonl and *.jsonl.gz file under root, and says how many folders below
// root could not be listed (those are skipped and the rest is read), or why root itself could not.
func Walk(root string, f func(string)) (unreadable int, err error) {
	if _, err := os.ReadDir(root); err != nil {
		return 0, fmt.Errorf("list %s: %w", root, err)
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			unreadable++
			return nil //nolint:nilerr // counted and reported; the rest of the tree is still read
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".jsonl") || strings.HasSuffix(p, ".jsonl.gz")) {
			f(p)
		}
		return nil
	})
	return unreadable, nil
}

// CommitSessions is the in-scope repo each agent session committed to, by session id, as the
// commit.recorded lines in dataDir's ledger files name them; a session that committed to more
// than one has its newest commit's. The post-commit hook writes that line only in an in-scope
// repo. unreadable counts the files and folders that exist but could not be read; their lines
// are missing.
func CommitSessions(dataDir string) (repos map[string]string, unreadable int) {
	newest := map[string]commit{}
	read := func(path string) {
		if commitsIn(path, newest) != nil {
			unreadable++
		}
	}
	read(filepath.Join(dataDir, ledger.FileName))
	roots := []string{filepath.Join(dataDir, ledger.SpillDir), filepath.Join(dataDir, ledger.SegmentsDir),
		filepath.Join(dataDir, ledger.UploadedDir)}
	if sink := LocalSink(); sink != "" {
		roots = append(roots, sink)
	}
	for _, root := range roots {
		n, err := Walk(root, read)
		unreadable += n
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			unreadable++
		}
	}
	repos = make(map[string]string, len(newest))
	for session, c := range newest {
		repos[session] = c.Repo
	}
	return repos, unreadable
}

// commit is the part of a commit.recorded line CommitSessions reads.
type commit struct {
	Event     string `json:"event"`
	TS        string `json:"ts"`
	Repo      string `json:"repo"`
	SessionID string `json:"session_id"`
	at        time.Time
}

// commitsIn adds path's commit.recorded lines to newest, keeping each session's newest. A file
// that does not exist adds none; one that cannot be opened or read through is an error.
func commitsIn(path string, newest map[string]commit) error {
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
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
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		if !strings.Contains(sc.Text(), eventRecorded) {
			continue
		}
		var c commit
		if json.Unmarshal(sc.Bytes(), &c) != nil || c.Event != eventRecorded || c.SessionID == "" || c.Repo == "" {
			continue
		}
		c.at, _ = time.Parse(time.RFC3339Nano, c.TS) // an unreadable time ranks oldest
		if have, ok := newest[c.SessionID]; !ok || c.at.After(have.at) {
			newest[c.SessionID] = c
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}
