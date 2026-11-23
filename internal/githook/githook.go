// Package githook is `livy git post-checkout <prev> <new> <flag>`: git's post-checkout
// hook, which records a work boundary as work.started. It lives in git, not in Claude
// Code, because a boundary must be recorded whoever switched: the agent, a terminal or an
// IDE.
//
// prev is ignored on purpose: branching from the current commit makes both SHAs name the
// new branch, so a previous branch read from it is wrong exactly when it matters, and the
// ledger's timeline already says where the work was.
package githook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
)

// PostCheckout handles one post-checkout call and returns the exit code, always 0. Git
// sets the flag to 1 for a branch checkout and 0 for a file checkout. It is also 1 for a
// detached checkout (a rebase runs detached) and for checking out the branch already
// checked out; neither is a work boundary, so neither is recorded.
func PostCheckout(args []string) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if len(args) != 3 || args[2] != "1" {
		return 0 // a file checkout: git asked nothing of us
	}
	timeout := hookTimeout()
	branch, err := git(timeout, "symbolic-ref", "-q", "--short", "HEAD")
	if errors.Is(err, context.DeadlineExceeded) {
		record("ledger.error", fmt.Sprintf("reason=post-checkout: git did not answer in %ss; branch switch not recorded",
			gitinfo.FormatSeconds(timeout.Seconds())))
		return 0
	}
	if err != nil || branch == "" {
		return 0 // detached, no git, or not a repository: nothing we measure happened
	}
	last, err := git(timeout, "reflog", "-1", "--format=%gs")
	if errors.Is(err, context.DeadlineExceeded) {
		record("ledger.error", fmt.Sprintf("reason=post-checkout: git did not answer in %ss; branch switch not recorded",
			gitinfo.FormatSeconds(timeout.Seconds())))
		return 0
	}
	if IsBoundary(args[2], branch, last) {
		record("work.started", "hook=post-checkout")
	}
	return 0
}

// IsBoundary decides whether a checkout is a work boundary: flag 1 (a branch checkout,
// not a file checkout), HEAD on a named branch (a rebase or --detach leaves it detached,
// branch ""), and not a checkout of the branch already checked out (the reflog's last
// move is from that branch to itself).
func IsBoundary(flag, branch, lastMove string) bool {
	return flag == "1" && branch != "" && lastMove != "checkout: moving from "+branch+" to "+branch
}

// record writes through the hook writer, which applies scope (an out-of-scope repo leaves
// no trace) and says on the ledger what it could not write. Its stderr is dropped, as git
// prints a hook's stderr into the engineer's checkout output.
func record(event string, override string) {
	null, err := os.Open(os.DevNull)
	if err == nil {
		defer null.Close()
	}
	hook.Run([]string{event, override}, null, io.Discard)
}

// git runs one git command in the working directory under timeout; the trimmed output.
func git(timeout time.Duration, args ...string) (string, error) {
	out, err := gitRun(timeout, nil, args...)
	return strings.TrimSpace(string(out)), err
}
