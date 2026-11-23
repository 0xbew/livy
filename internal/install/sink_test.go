package install

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// Guards: a first install with `"path": "~/livy-data"` in the profile exited 0 and printed no
// sink line, so the engineer learned only from doctor (or never) that every upload is refused.
func TestInstallFailsOnASinkEveryEmitRefuses(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"LIVY_DIR", oldnames.LivyEnv("DIR"), oldnames.RecorderEnv("DIR"), "CLAUDE_CONFIG_DIR", "CODEX_HOME",
		"LIVY_SINK_TYPE", "LIVY_SINK_BUCKET"} {
		if k == "" {
			continue
		}
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HOME", home)
	t.Setenv("LIVY_LINK_DIR", filepath.Join(home, "linkbin"))
	profile := filepath.Join(home, "profile.json")
	body := `{"scope": {"orgs": ["acme"]}, "sink": {"type": "local", "path": "~/livy-data"}}`
	if err := os.WriteFile(profile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := Install([]string{"--no-schedule", "--no-discover", "--config", profile}, &out, io.Discard)
	if code != 1 {
		t.Errorf("install exited %d, want 1", code)
	}
	if !strings.Contains(out.String(), "sink: ") {
		t.Errorf("install said nothing about the sink:\n%s", out.String())
	}
}

// Guards: a machine installed with no upload destination keeping everything in the data
// folder until the OTEL stream hits its cap and refuses exports, a default that overrides a sink
// someone set, and a data folder other users can read.
func TestNoSinkDefaultsToALocalFolder(t *testing.T) {
	cases := []struct {
		name, config string
		env          map[string]string
		wantPath     bool // the config's sink is the local default afterwards
	}{
		{"nothing set", "", nil, true},
		{"a sink in the config", `{"sink":{"type":"gcs","bucket":"team-raw"}}`, nil, false},
		{"a sink from the environment", "", map[string]string{"LIVY_SINK_TYPE": "s3", "LIVY_SINK_BUCKET": "b"}, false},
		{"a re-install of the default", `{"sink":{"type":"local","path":"/elsewhere/kept"}}`, nil, false},
	}
	for _, c := range cases {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("LIVY_DIR", filepath.Join(home, ".livy"))
		t.Setenv("LIVY_SINK_TYPE", "")
		t.Setenv("LIVY_SINK_BUCKET", "")
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		cfg := filepath.Join(home, ".livy", "config.json")
		if c.config != "" {
			if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg, []byte(c.config), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ensureSink(home, false, ""); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		raw, _ := os.ReadFile(cfg)
		want := filepath.Join(home, "livy-data")
		if got := strings.Contains(string(raw), `"path": "`+want+`"`); got != c.wantPath {
			t.Errorf("%s: config names the local default %v, want %v:\n%s", c.name, got, c.wantPath, raw)
		}
		st, err := os.Stat(want)
		if c.wantPath && (err != nil || st.Mode().Perm() != 0o700) {
			t.Errorf("%s: %s mode %v (%v), want 0700", c.name, want, st, err)
		}
		if !c.wantPath && err == nil {
			t.Errorf("%s: made %s although a sink was set", c.name, want)
		}
	}
}
