package doctor

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/0xbew/livy/internal/install"
)

// codexMustMatch is what a Codex group's matcher must let through for Livy's hook there to
// record anything: sessions from a fresh start, tool calls from every tool.
var codexMustMatch = map[string]string{"SessionStart": "startup", "PostToolUse": "Bash"}

// checkCodexHooks is the `codex hooks` row, when install wrote Livy's entries to Codex's
// hooks file. Whether Codex trusts them is Codex's own record, against each hook's hash, and
// Livy cannot check it, so the row says how to.
func checkCodexHooks(r *Report) {
	st, _ := install.Recorded()
	if st.CodexHooks == "" {
		return
	}
	raw, err := os.ReadFile(st.CodexHooks)
	if err != nil {
		r.add(Fail, "codex hooks", "%s unreadable (%v): no Codex hook runs Livy; run `livy install`", st.CodexHooks, err)
		return
	}
	var doc struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.add(Fail, "codex hooks", "%s is not the hooks shape Livy reads (%v)", st.CodexHooks, err)
		return
	}
	missing, bins := livyHooks(doc.Hooks, install.CodexRequired, codexMustMatch)
	if len(missing) > 0 {
		r.add(Fail, "codex hooks", "no Livy hook for %s in %s; run `livy install`", strings.Join(missing, ", "), st.CodexHooks)
		return
	}
	for bin := range bins {
		if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
			r.add(Fail, "codex hooks", "the hooks run %s, which is missing or not executable", bin)
			return
		}
	}
	r.add(OK, "codex hooks", "all %d events in %s; Codex runs them only once trusted (/hooks in Codex)", len(install.CodexRequired), st.CodexHooks)
}
