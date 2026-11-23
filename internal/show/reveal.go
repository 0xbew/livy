package show

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/prompts"
	"github.com/0xbew/livy/internal/vault"
)

var (
	// ErrNoDenylist is a reveal refused: text is shown only after the scrubber has the identity list.
	ErrNoDenylist = errors.New("refused: no usable denylist; run `livy install --refresh-denylist` or set denylist.path")
	// ErrNotLogged is a reveal whose prompt.revealed line could not be written: nothing is shown.
	ErrNotLogged = errors.New("the reveal could not be written to the ledger")
)

// Excerpt is one prompt's kept text, repo and branch as they would be shared.
type Excerpt struct {
	PromptID, HarnessPromptID, SessionID string
	Repo, Branch                         string
	TS                                   time.Time
	Text                                 string
}

// Label is the excerpt's prompt id, with the harness's own id beside it when there is one.
func (e Excerpt) Label() string {
	if e.HarnessPromptID == "" {
		return e.PromptID
	}
	return e.PromptID + " (" + e.HarnessPromptID + ")"
}

// Revealed is what one reveal shows, what the audit refused (with its rules), and the ids the
// vault does not hold.
type Revealed struct {
	Shown           []Excerpt
	Refused         map[string][]string
	Missing         []string
	DenylistVersion int
	// Unread is why part of the vault could not be read, nil when all of it was: Missing may
	// then hold ids that are kept in the part not read.
	Unread error
}

// Reveal is `livy show`'s path for ids, the prompts behind commit sha ("" for none): their kept
// text scrubbed whole with the denylist and audited, after one prompt.revealed line per repo
// (ids and sha, never text) is written. It refuses without a denylist, and shows nothing when the
// reveal cannot be logged.
func Reveal(ids []string, sha string, stderr io.Writer) (Revealed, error) {
	names, why := prompts.LoadDenylist()
	if names == nil {
		return Revealed{}, fmt.Errorf("%w (%s)", ErrNoDenylist, why)
	}
	recs, unread := vault.Find(hook.DataDir(), ids)
	j := judge(recs, names)
	if len(j.shown) > 0 && !logReveal(j.shown, sha, names, stderr) {
		return Revealed{}, ErrNotLogged
	}
	out := Revealed{Refused: j.refused, Missing: missing(ids, recs), DenylistVersion: names.Version, Unread: unread}
	for _, s := range j.shown {
		out.Shown = append(out.Shown, Excerpt{PromptID: s.rec.PromptID, HarnessPromptID: s.rec.HarnessPromptID,
			SessionID: s.rec.SessionID, Repo: s.repo, Branch: s.branch, TS: s.rec.TS, Text: s.text})
	}
	return out, nil
}
