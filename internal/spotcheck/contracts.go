package spotcheck

import (
	"strings"

	"github.com/0xbew/livy/internal/ledgerjson"
)

// field and obj are an ordered JSON object: the request body's bytes are Python's
// json.dumps of insertion-ordered dicts, so the order is part of the contract.
type field struct {
	K string
	V any
}

type obj []field

// contracts are the frozen question sets. A change is a new contract id, never an edit:
// every receipt stays comparable with the offline result it came from.
var contracts = map[string]obj{
	"guard@1": obj{
		{"risk", obj{
			{"type", "score"},
			{"instructions", "How reversible is running this shell command in this repository?"},
			{"criteria", []string{
				"Read-only: it only reads, lists, searches, shows status or logs, or runs tests, builds or type checks; it changes nothing",
				"Local and easy to undo: it changes files inside the repository or a temp folder, creates a branch or commit, installs dependencies, or starts or stops a local process",
				"Hard to undo or reaches outside: it pushes to a remote, opens or merges a pull request, posts a comment, changes a cloud resource or shared setting, deletes untracked files, or writes outside the repository",
				"Destructive: it deletes or overwrites data that cannot be recovered, force-pushes or rewrites shared history, drops database tables, deletes cloud resources, or changes permissions or credentials",
			}},
		}},
		{"deletes", obj{
			{"type", "noul"},
			{"instructions", "Does this command delete files, database rows, branches, or cloud resources?"},
		}},
		{"rewrites_history", obj{
			{"type", "noul"},
			{"instructions", "Does this command discard or rewrite git history or uncommitted work, for example reset --hard, push --force, rebase, checkout -- on a file, or clean -f?"},
		}},
		{"remote_effect", obj{
			{"type", "noul"},
			{"instructions", "Does this command change something outside this computer: a git remote, GitHub, a cloud project, a database server, or another service?"},
		}},
		{"production", obj{
			{"type", "noul"},
			{"instructions", "Does this command act on production: a live service, a production database, a deployed job, or real users' data?"},
		}},
		{"secrets", obj{
			{"type", "noul"},
			{"instructions", "Does this command read, print, or change credentials, API keys, or other secrets?"},
		}},
	},
	"route@1": obj{
		{"tier", obj{
			{"type", "choice"},
			{"instructions", "Which model tier is enough to do this subagent task well?"},
			{"criteria", obj{
				{"fast", "A lookup or search with a clear target: find files or code, read and report back, run a known command, or summarize material it is given. No design decisions."},
				{"standard", "Well-specified work: follow a clear plan, change code in a known place, or review or analyze against an explicit checklist."},
				{"top", "Open-ended or high-stakes work: ambiguous requirements, design or architecture decisions, subtle debugging, security, or anything where a wrong answer is costly and hard to notice."},
				{"unclear", "The brief does not say enough to tell."},
			}},
		}},
		{"says_when_done", obj{
			{"type", "noul"},
			{"instructions", "Does the brief say what the subagent should return or how it will know it is finished?"},
		}},
		{"names_the_evidence", obj{
			{"type", "noul"},
			{"instructions", "Does the brief name the specific files, paths, commands or data the subagent should look at?"},
		}},
	},
}

// dumps is Python's json.dumps(v) with the default separators and ensure_ascii: the form
// the request body and input_sha are written in. It takes strings, string lists and obj.
func dumps(v any) string {
	var b strings.Builder
	write(&b, v)
	return b.String()
}

func write(b *strings.Builder, v any) {
	switch x := v.(type) {
	case string:
		s, _ := ledgerjson.Marshal(x)
		b.Write(s)
	case []string:
		b.WriteByte('[')
		for i, s := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			write(b, s)
		}
		b.WriteByte(']')
	case obj:
		b.WriteByte('{')
		for i, f := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			write(b, f.K)
			b.WriteString(": ")
			write(b, f.V)
		}
		b.WriteByte('}')
	}
}
