package githook

import "testing"

// Guards: false work boundaries. git sets the flag to 1 for a rebase (HEAD detached), for
// --detach, and for checking out the branch already checked out; each would write a
// work.started that never happened. Each row's reflog line is git's own
// text for that operation, read from a real repository.
func TestIsBoundary(t *testing.T) {
	for _, c := range []struct {
		name, flag, branch, lastMove string
		want                         bool
	}{
		{"switch to another branch", "1", "feature/y", "checkout: moving from main to feature/y", true},
		{"the branch already checked out", "1", "feature/y", "checkout: moving from feature/y to feature/y", false},
		{"file checkout", "0", "feature/y", "checkout: moving from main to feature/y", false},
		{"detached (rebase, --detach)", "1", "", "checkout: moving from main to 0123abc", false},
		{"worktree add -b (its reflog line is not a checkout)", "1", "feature/z", "reset: moving to HEAD", true},
	} {
		if got := IsBoundary(c.flag, c.branch, c.lastMove); got != c.want {
			t.Errorf("%s: IsBoundary(%q, %q, %q) = %v, want %v", c.name, c.flag, c.branch, c.lastMove, got, c.want)
		}
	}
}
