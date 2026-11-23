//go:build unix

package install

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/oldnames"
)

// Guards: the data folder and ledger were created 0755/0644, so on a shared machine every
// user could read the repos, branches and session ids Livy records; and a ~/.livy made
// 0755 by an earlier install stayed that way.
func TestDataFolderAndLedgerArePrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	for _, tc := range []struct {
		name       string
		existing   bool // ~/.livy already there, 0755
		unwritable bool // LIVY_DIR names a folder that cannot be created
	}{{"fresh install", false, false}, {"earlier install left it 0755", true, false},
		{"a data folder that cannot be made private fails the install", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, k := range []string{"LIVY_DIR", oldnames.LivyEnv("DIR"), oldnames.RecorderEnv("DIR"), "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
				if k == "" {
					continue
				}
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			t.Setenv("HOME", home)
			t.Setenv("LIVY_LINK_DIR", filepath.Join(home, "linkbin"))
			if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(home, ".livy")
			if tc.existing {
				if err := os.Mkdir(data, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.unwritable {
				locked := filepath.Join(home, "locked")
				if err := os.Mkdir(locked, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(locked, 0o700) })
				t.Setenv("LIVY_DIR", filepath.Join(locked, "data"))
				if code := Install([]string{"--no-schedule", "--no-discover", "--org", "acme"}, io.Discard, io.Discard); code == 0 {
					t.Errorf("install exited 0 though %s could not be created private", filepath.Join(locked, "data"))
				}
				return
			}
			if code := Install([]string{"--no-schedule", "--no-discover", "--org", "acme"}, io.Discard, io.Discard); code != 0 {
				t.Fatalf("install exited %d", code)
			}
			if _, err := ledger.Append(hook.DataDir(), []byte("{}\n")); err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]os.FileMode{data: 0o700, filepath.Join(data, ledger.FileName): 0o600} {
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := st.Mode().Perm(); got != want {
					t.Errorf("%s is %#o, want %#o", path, got, want)
				}
			}
		})
	}
}
