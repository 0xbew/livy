package hook

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/ledger"
)

// Recorder is the hook's machinery for another hook that runs in the same data folder and
// repository scope and writes its own event: the scope check, the payload read under a
// deadline, the repo and branch, the tool call's target, and the ledger writer. It lives
// here so that hook's import graph stays free of networking: the hook that needs the network
// (spotcheck, async) imports this and carries the network itself.
type Recorder struct{ r *run }

// NewRecorder is a recorder for this process.
func NewRecorder(stderr io.Writer) *Recorder {
	r := &run{stderr: stderr, git: gitinfo.New(stderr), path: filepath.Join(dataDir(), ledger.FileName)}
	r.git.Hosts = hosts(stderr)
	return &Recorder{r}
}

// InScope reports whether the working repository's origin is under a configured org.
func (c *Recorder) InScope() bool { return c.r.inScope() }

// GitTimeout is how long git was given, and whether it ran out of it asking for scope.
func (c *Recorder) GitTimeout() (seconds string, timedOut bool) {
	return gitinfo.FormatSeconds(c.r.git.Timeout()), c.r.git.TimedOut()
}

// Payload reads the hook payload without ever blocking: the object (nil when none came) and
// why it could not be used ("" when it could).
func (c *Recorder) Payload(stdin *os.File) (obj map[string]any, problem string) {
	p := readStdin(stdin)
	return p.obj, p.problem
}

// Event starts a record of the named event for this repo and branch.
func (c *Recorder) Event(name string) map[string]any {
	rec := c.r.base(name)
	rec["repo"] = c.r.git.Slug()
	rec["branch"] = c.r.branch(name)
	return rec
}

// Toplevel is the working repository's root folder, or "".
func (c *Recorder) Toplevel() string { return c.r.git.Toplevel() }

// Slug is the working repository as owner/name.
func (c *Recorder) Slug() string { return c.r.git.Slug() }

// Target adds where the tool call in obj worked (target_via, target_repo, target_branch).
func (c *Recorder) Target(rec, obj map[string]any) { c.r.target(rec, obj) }

// Append validates rec against the schema and appends it to the ledger; false means it was
// written nowhere.
func (c *Recorder) Append(rec map[string]any) bool { return c.r.append(rec) }

// Error writes a ledger.error. With askGit false it carries no repo, as a git timeout's does.
func (c *Recorder) Error(reason string, askGit bool) { c.r.errorLine(reason, askGit, false) }

// GitTimedOutError says that git ran out of time confirming scope, naming what was lost.
func (c *Recorder) GitTimedOutError(who, lost string) {
	seconds, _ := c.GitTimeout()
	c.Error(fmt.Sprintf("%sgit timed out after %ss; repo scope unconfirmed, %s", who, seconds, lost), false)
}
