package install

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// Guards: install and uninstall over a settings file the Python installer wrote (json.dump
// escapes non-ASCII as \uXXXX) rewrote the engineer's own entries with raw UTF-8, so the
// file was not given back as it was (rules 22, 23). Expected value: the file the real
// Python installer wrote (testdata, captured from a temp HOME).
func TestInstallThenUninstallKeepsAPythonWrittenFileByteForByte(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "fakebin")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"LIVY_DIR", oldnames.LivyEnv("DIR"), oldnames.RecorderEnv("DIR"), "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		if k == "" {
			continue
		}
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HOME", home)
	t.Setenv("LIVY_LINK_DIR", filepath.Join(home, "linkbin"))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	original, err := os.ReadFile("testdata/python_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(settings, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Install([]string{"--no-schedule", "--no-discover", "--org", "acme"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("install exited %d", code)
	}
	if code := Uninstall(nil, io.Discard, io.Discard); code != 0 {
		t.Fatalf("uninstall exited %d", code)
	}
	if got, _ := os.ReadFile(settings); !bytes.Equal(got, original) {
		t.Errorf("after install then uninstall the file is not the one the Python installer wrote:\n%s", got)
	}
}
