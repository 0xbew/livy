package install

import (
	"path/filepath"
	"slices"
	"testing"
)

// Guards: the owner's loop. A repo an earlier install hooked, but never seen again in a Claude
// Code transcript, never got the commit hooks a later version added, so doctor's "run livy
// install" could not fix it; and `livy install` run inside a repo skipping that repo.
func TestInstallRevisitsTheReposItHookedAndTheOneItRunsIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(home, ".livy"))
	t.Setenv("LIVY_SCOPE_ORGS", "acme")
	repoAt := func(origin string) string {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		gitIn(dir, "init", "-q")
		gitIn(dir, "remote", "add", "origin", origin)
		return dir
	}
	hooked := repoAt("https://github.com/acme/hooked.git")
	redirected := repoAt("https://github.com/acme/redirected.git")
	here := repoAt("https://github.com/acme/here.git")
	outside := repoAt("https://github.com/other/hooked.git")
	prev := State{
		GitHooks:      []string{filepath.Join(hooked, ".git", "hooks", "post-checkout"), filepath.Join(outside, ".git", "hooks", "post-checkout")},
		HookRedirects: []hookRedirect{{GitDir: filepath.Join(redirected, ".git")}},
	}
	t.Chdir(here)
	got := installRepos(options{noDiscover: true}, paths{home: home, settings: filepath.Join(home, ".claude", "settings.json")}, prev)
	for _, want := range []string{hooked, redirected, here} {
		if !slices.Contains(got, want) {
			t.Errorf("install's repos %v leave out %s", got, want)
		}
	}
	if slices.Contains(got, outside) {
		t.Errorf("install's repos %v take %s, whose origin is out of scope", got, outside)
	}
}
