// Package keyspace is Livy's local index: the ledger's lines and the OTEL stream's spans kept a
// second time in small append-only files under one predictable root, keyed by what is known when
// they are written (a session, a repo's branch, a commit sha), so one pull request's lines are
// read by name instead of by scanning everything. The day segments stay the record; the index
// is derived from them and `livy reindex` rebuilds it.
//
// Layout, under the data folder:
//
//	index/CURRENT                                  the live generation's folder name
//	index/lock                                     held by every write and by a rebuild
//	index/stale                                    a write that could not be kept anywhere
//	index/pending/                                 lines a writer could not index in time
//	index/<generation>/built.json                  what built it (Manifest)
//	index/<generation>/ledger/session/<hh>/<id>.jsonl
//	index/<generation>/ledger/branch/<owner>__<repo>/<branch>.jsonl
//	index/<generation>/ledger/commit/<sha[0:2]>.jsonl
//	index/<generation>/otel/session/<hh>/<id>.jsonl   compact spans naming the session
//	index/<generation>/otel/trace/<hh>.jsonl          which sessions each trace names
//	index/<generation>/otel/orphan/<hh>.jsonl         compact spans naming no session
//
// <hh> is the first two hex digits of the key's sha256, so ids that share a prefix still spread
// over 256 folders.
package keyspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// Names inside the data folder's index folder.
const (
	DirName      = "index"
	currentName  = "CURRENT"
	lockName     = "lock"
	staleName    = "stale"
	pendingName  = "pending"
	manifestName = "built.json"
	genPrefix    = "g-"
)

// Schema names the index layout. A generation built under another value is not read.
const Schema = "livy.index/1"

// Folders inside a generation.
const (
	ledgerSessions = "ledger/session"
	ledgerBranches = "ledger/branch"
	ledgerCommits  = "ledger/commit"
	otelSessions   = "otel/session"
	otelTraces     = "otel/trace"
	otelOrphans    = "otel/orphan"
	ext            = ".jsonl"
)

// SessionDir and CommitDir are where a generation keeps session files and commit buckets, for
// a reader that orders what it read by kind.
var (
	SessionDir = filepath.FromSlash(ledgerSessions)
	CommitDir  = filepath.FromSlash(ledgerCommits)
)

// maxName is the longest escaped file name kept whole; a longer one is cut and ends in a hash.
const maxName = 200

// Events a branch file keeps: the lines that say which commits and checkouts belong to it.
const (
	eventStarted   = "work.started"
	eventRecorded  = "commit.recorded"
	eventRewritten = "commit.rewritten"
	eventPROpened  = "work.pr_opened"
)

// DataNames are what this package keeps in the data folder, by exact name: `livy uninstall
// --purge` deletes these.
func DataNames() []string { return []string{DirName} }

// Dir is the index folder of a data folder.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

// Keys are the fields of a ledger line the index files it under.
type Keys struct {
	Event        string `json:"event"`
	SessionID    string `json:"session_id"`
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	CommitSHA    string `json:"commit_sha"`
	OldCommitSHA string `json:"old_commit_sha"`
}

// KeysOf reads a ledger line's keys; false when it is not a JSON object.
func KeysOf(line []byte) (Keys, bool) {
	var k Keys
	if json.Unmarshal(line, &k) != nil {
		return Keys{}, false
	}
	return k, true
}

// ledgerFiles is where a ledger line goes, relative to a generation: its session's file, its
// branch's when it is a boundary line, and the commit buckets of the shas a commit line names.
// None for a line with neither a session nor a branch boundary.
func ledgerFiles(k Keys) []string {
	var out []string
	if k.SessionID != "" {
		out = append(out, SessionFile(k.SessionID))
	}
	switch k.Event {
	case eventStarted, eventRecorded, eventRewritten, eventPROpened:
		if k.Repo != "" && k.Branch != "" {
			out = append(out, BranchFile(k.Repo, k.Branch))
		}
	}
	if k.Event == eventRecorded || k.Event == eventRewritten {
		for _, sha := range []string{k.CommitSHA, k.OldCommitSHA} {
			if b := CommitBucket(sha); sha != "" && !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
	}
	return out
}

// SessionFile is a session's ledger file, relative to a generation.
func SessionFile(id string) string { return filepath.Join(ledgerSessions, shard(id), name(id)+ext) }

// BranchFile is a repo's branch file, relative to a generation. The repo is lowercased: GitHub
// names are case-insensitive and the ledger keeps what the remote URL said.
func BranchFile(repo, branch string) string {
	owner, rest, ok := strings.Cut(strings.ToLower(repo), "/")
	dir := name(owner)
	if ok {
		dir += "__" + name(rest)
	}
	return filepath.Join(ledgerBranches, dir, name(branch)+ext)
}

// CommitBucket is the bucket a commit sha's lines are in, relative to a generation: its first
// two hex digits, or the sha256 shard of anything that is not a sha.
func CommitBucket(sha string) string {
	s := strings.ToLower(sha)
	if len(s) >= 2 && isHex(s[:2]) {
		return filepath.Join(ledgerCommits, s[:2]+ext)
	}
	return filepath.Join(ledgerCommits, shard(sha)+ext)
}

// SpanFile is a session's span file, relative to a generation.
func SpanFile(id string) string { return filepath.Join(otelSessions, shard(id), name(id)+ext) }

// TraceBucket is the bucket that says which sessions a trace names, relative to a generation.
func TraceBucket(trace string) string { return filepath.Join(otelTraces, shard(trace)+ext) }

// OrphanBucket is the bucket of a trace's spans that name no session, relative to a generation.
func OrphanBucket(trace string) string { return filepath.Join(otelOrphans, shard(trace)+ext) }

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// shard is the first two hex digits of key's sha256.
func shard(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:1])
}

// name is key as a file name: bytes outside [A-Za-z0-9._-] and a leading dot as %XX, and a name
// past maxName cut and ended with a hash of the whole key, so two long keys never share a file.
func name(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' && i > 0
		if plain {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	out := b.String()
	if out == "" {
		return "%00"
	}
	if len(out) > maxName {
		sum := sha256.Sum256([]byte(key))
		out = out[:maxName-17] + "~" + hex.EncodeToString(sum[:8])
	}
	return out
}
