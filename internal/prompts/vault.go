package prompts

import (
	"fmt"

	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/ledgerfiles"
	"github.com/0xbew/livy/internal/vault"
)

// keepText keeps each new prompt's text in the local vault, whether or not upload is on, and
// prunes what is past the vault's days. What it could not do is a ledger.error. Its counts stay
// off the pass's stdout, which keeps the Python tool's shape.
func keepText(o Options, w *extract.Writer, prompts []claudecode.Prompt) vault.Kept {
	if _, err := vault.Prune(o.Dir, o.Now); err != nil {
		w.Error(fmt.Sprintf("prompt_vault: could not prune texts older than %d days (%v)", vault.KeepDays, err))
	}
	recs := make([]vault.Record, 0, len(prompts))
	for _, pr := range prompts {
		r := vault.Record{PromptID: pr.ID, HarnessPromptID: pr.HarnessID, SessionID: pr.SessionID, Repo: pr.Repo,
			Branch: pr.Branch, Text: pr.Text}
		if t, ok := when(pr.TS); ok {
			r.TS = t
		}
		recs = append(recs, r)
	}
	k, err := vault.Keep(o.Dir, recs, o.Now)
	if err != nil {
		w.Error(fmt.Sprintf("prompt_vault: %d prompt text(s) not kept (%v)", len(recs)-k.Kept-k.Already-k.Empty-k.TooOld, err))
	}
	for _, why := range k.Unchecked {
		w.Error(fmt.Sprintf("prompt_vault: a day file's last line could not be checked for a torn write (%v); its new prompt texts were kept without it", why))
	}
	if k.Empty > 0 {
		w.Error(fmt.Sprintf("prompt_vault: %d prompt(s) read with no id, time or text, not kept: the transcript format may have changed", k.Empty))
	}
	return k
}

// committedOnly is the prompts read outside every in-scope repo whose session committed to one,
// as the ledger files in o.Dir record it, each under that repo: a session started outside a
// repo that then commits to an in-scope one (a scratch workspace) is one piece of work, so every
// prompt of it is kept, and a session that never committed to one is not. Ledger files it could
// not read are a ledger.error: a session they name loses its outside prompts until they can be
// read.
func committedOnly(o Options, w *extract.Writer, outside []claudecode.Prompt) []claudecode.Prompt {
	if len(outside) == 0 {
		return nil
	}
	repos, unreadable := ledgerfiles.CommitSessions(o.Dir)
	if unreadable > 0 {
		w.Error(fmt.Sprintf("prompt_vault: %d ledger file(s) or folder(s) could not be read for commit.recorded; "+
			"prompts sent outside a repo by the sessions they name are not kept this run", unreadable))
	}
	var out []claudecode.Prompt
	for _, pr := range outside {
		if repo := repos[pr.SessionID]; repo != "" {
			pr.Repo = repo
			out = append(out, pr)
		}
	}
	return out
}
