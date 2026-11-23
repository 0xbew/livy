package install

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// Guards: --no-codex-otel refused or ignored beside an OTEL destination. Python keeps Claude Code's
// export and drops Codex's; so must this. Expected: settings.json names the endpoint, config.toml has no [otel].
func TestNoCodexOtelSuppressesOnlyCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", home+"/data")
	if k := oldnames.RecorderEnv("DIR"); k != "" {
		t.Setenv(k, home+"/data")
	}
	t.Setenv("LIVY_LINK_DIR", home+"/bin") // never the real /usr/local/bin or ~/.local/bin
	if err := os.MkdirAll(home+"/.codex", 0o700); err != nil {
		t.Fatal(err)
	}
	Install([]string{"--orgs", "acme", "--no-launch-agent", "--no-discover", "--otel-endpoint", "https://otel.example.test",
		"--no-codex-otel"}, io.Discard, io.Discard)
	settings, _ := os.ReadFile(filepath.Join(home, ".claude/settings.json"))
	if !strings.Contains(string(settings), "https://otel.example.test") {
		t.Errorf("Claude Code's export was dropped:\n%s", settings)
	}
	codex, _ := os.ReadFile(filepath.Join(home, ".codex/config.toml"))
	if strings.Contains(string(codex), "[otel]") {
		t.Errorf("Codex still exports:\n%s", codex)
	}
}

// Guards: an explicit --no-codex-otel=false read as true, silently dropping Codex's export.
func TestAnExplicitFalseNoCodexOtelIsKept(t *testing.T) {
	got := translatePyArgs([]string{"--no-codex-otel=false"})
	if len(got) != 1 || got[0] != "--no-codex-otel=false" {
		t.Errorf("translated to %v", got)
	}
}
