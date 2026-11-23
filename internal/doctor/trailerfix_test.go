package doctor

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Guards: doctor advising a command that cannot be run as printed. For a repo gone out of scope
// the way out is uninstalling it, and that command needs the repo's path.
func TestTrailerFixNamesTheRepoToUninstall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "remote", "add", "origin", "https://github.com/other/x.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	got := trailerFix([]string{filepath.Join(repo, ".git", "hooks")}, "/x/livy")
	if want := "livy uninstall --repo " + repo; !strings.Contains(got, want) {
		t.Errorf("trailerFix = %q, want it to name %q", got, want)
	}
}
