package doctor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/ledger"
)

// outsideWindow is how far back doctor looks for sessions run from outside the repo: files
// changed in it, and sessions with a model call in it.
const outsideWindow = 7 * 24 * time.Hour

// Ledger events and fields checkOutsideRepo reads.
const (
	eventAPICall     = "usage.api_call"
	eventCallDetail  = "call.detail"
	eventPrompt      = "prompt.submitted"
	eventInteraction = "interaction"
	kindPrompt       = "prompt"
	harnessClaude    = "claude-code"
	repoUnknown      = "unknown"
)

// outsideLine is the part of a ledger line checkOutsideRepo reads.
type outsideLine struct {
	Event               string `json:"event"`
	TS                  string `json:"ts"`
	SessionID           string `json:"session_id"`
	Repo                string `json:"repo"`
	Harness             string `json:"harness"`
	Kind                string `json:"kind"`
	PromptInteractionID string `json:"prompt_interaction_id"`
}

// outsideSession is what one session's lines say: the repos its model calls in the window worked
// in, and whether anything shows a prompt was recorded for it.
type outsideSession struct {
	repos    map[string]bool
	prompted bool
}

// checkOutsideRepo warns about recent Claude Code sessions that worked in an in-scope repo from a
// folder outside it. Their hooks, prompt.submitted and turn.ended among them, scope by the
// process's folder and never write, and extraction drops the prompt interaction by the record's
// folder, while the calls and tools land in the repo they name: such a session has model calls
// and no prompt line, and no call.detail tied to a prompt. No row when there is none.
func checkOutsideRepo(r *Report, dir string) {
	byRepo, total := outsideSessions(dir, clock.Now())
	if total == 0 {
		return
	}
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	slices.SortFunc(repos, func(a, b string) int {
		if byRepo[a] != byRepo[b] {
			return byRepo[b] - byRepo[a]
		}
		return strings.Compare(a, b)
	})
	where, it := repos[0], "it"
	if len(repos) > 1 {
		parts := make([]string, 0, len(repos))
		for _, repo := range repos {
			parts = append(parts, fmt.Sprintf("%s (%d)", repo, byRepo[repo]))
		}
		where, it = strings.Join(parts, ", "), "those repos"
	}
	r.add(Warn, "prompt capture", "%d Claude Code session(s) worked in %s from a folder outside %s: Livy has their calls "+
		"and tools but not their prompts, so their turns are inferred. Start Claude Code inside the repo to record prompts",
		total, where, it)
}

// outsideSessions is, per repo, how many Claude Code sessions with a model call in the last
// outsideWindow worked in it with no prompt recorded, and how many such sessions there are. It
// reads the ledger lines still on this machine (the ledger, its spill, pending segments and the
// copies kept after upload) in files changed in the window.
func outsideSessions(dir string, now time.Time) (byRepo map[string]int, total int) {
	since := now.Add(-outsideWindow)
	sessions := map[string]*outsideSession{}
	read := func(path string) {
		if st, err := os.Stat(path); err == nil && !st.ModTime().Before(since) {
			readOutside(path, since, sessions)
		}
	}
	read(filepath.Join(dir, ledger.FileName))
	for _, sub := range []string{ledger.SpillDir, ledger.SegmentsDir, ledger.UploadedDir} {
		_ = filepath.WalkDir(filepath.Join(dir, sub), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (strings.HasSuffix(p, ".jsonl") || strings.HasSuffix(p, ".jsonl.gz")) {
				read(p)
			}
			return nil //nolint:nilerr // best effort: an unreadable folder's lines go unread, the rest is counted
		})
	}
	byRepo = map[string]int{}
	for _, s := range sessions {
		if s.prompted || len(s.repos) == 0 {
			continue
		}
		total++
		for repo := range s.repos {
			byRepo[repo]++
		}
	}
	return byRepo, total
}

// readOutside adds path's lines to sessions. A file that cannot be read adds what was read.
func readOutside(path string, since time.Time, sessions map[string]*outsideSession) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = fh.Close() }() // opened read-only: a close error loses nothing
	var in io.Reader = fh
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(fh)
		if err != nil {
			return
		}
		in = gz
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		// Most lines are none of these; skip them before decoding.
		if !bytes.Contains(b, quoted(eventAPICall)) && !bytes.Contains(b, quoted(eventCallDetail)) &&
			!bytes.Contains(b, quoted(eventPrompt)) && !bytes.Contains(b, quoted(eventInteraction)) {
			continue
		}
		var l outsideLine
		if json.Unmarshal(b, &l) != nil || l.SessionID == "" {
			continue
		}
		addOutside(sessions, l, since)
	}
}

// quoted is s as a JSON string, so "interaction" does not match prompt_interaction_id.
func quoted(s string) []byte { return []byte(`"` + s + `"`) }

// addOutside adds one line to its session's tally.
func addOutside(sessions map[string]*outsideSession, l outsideLine, since time.Time) {
	s := sessions[l.SessionID]
	if s == nil {
		s = &outsideSession{repos: map[string]bool{}}
		sessions[l.SessionID] = s
	}
	switch {
	case l.Event == eventAPICall && (l.Harness == "" || l.Harness == harnessClaude):
		at, err := time.Parse(time.RFC3339, l.TS)
		if err == nil && !at.Before(since) && l.Repo != "" && l.Repo != repoUnknown {
			s.repos[l.Repo] = true
		}
	case l.Event == eventAPICall:
		s.prompted = true // another harness's session: not this check's
	case l.Event == eventPrompt, l.Event == eventInteraction && l.Kind == kindPrompt,
		l.Event == eventCallDetail && l.PromptInteractionID != "":
		s.prompted = true
	}
}
