package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xbew/livy/internal/relay"
)

// Guards: Codex's table naming a relay that is not running (every export lost, nothing
// said), and a re-install minting a new relay key, which locks out every Codex process
// already running with the old one.
func TestCodexTableNamesTheRelayOnlyOnceItLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LIVY_DIR", filepath.Join(home, ".livy"))
	codex := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, []byte("model = \"gpt-6\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := paths{home: home, bin: "/x/.livy/bin/livy", codex: codex}
	o := options{otel: "https://collector.example", otelAuth: "gcloud", handle: "eng"}
	oldHost, oldLoad, oldListening := relayHost, loadRelay, relayListening
	t.Cleanup(func() { relayHost, loadRelay, relayListening = oldHost, oldLoad, oldListening })
	relayHost = func() string { return "launchd" }
	relayListening = func(time.Time) bool { return true }

	loadRelay = func(string, string, string, []string) (Schedule, error) { return Schedule{}, errors.New("refused") }
	var st State
	if _, bad := codexOtel(p, o, State{}, &st); !bad {
		t.Error("a relay that did not load was not reported as a failed install")
	}
	if text, _ := os.ReadFile(codex); strings.Contains(string(text), RelayListen) || st.CodexRelay.Kind != "" {
		t.Errorf("the table names a relay that never loaded:\n%s", text)
	}

	loadRelay = func(string, string, string, []string) (Schedule, error) { return Schedule{Kind: "launchd"}, nil }
	if _, bad := codexOtel(p, o, State{}, &st); bad {
		t.Fatal("install with a loaded relay failed")
	}
	key := relay.ReadKey(filepath.Join(home, ".livy"))
	text, _ := os.ReadFile(codex)
	if key == "" || !strings.Contains(string(text), RelayListen) || !strings.Contains(string(text), key) {
		t.Fatalf("the table does not send to the relay with its key %q:\n%s", key, text)
	}
	again := st
	if _, bad := codexOtel(p, o, st, &again); bad || relay.ReadKey(filepath.Join(home, ".livy")) != key {
		t.Error("a re-install changed the relay key")
	}
}

// Guards: a fresh install that records no OTEL at all (the owner's "from day one"), an opt-out
// that a re-install quietly turns back on, and a receiver turned on where nothing can keep it
// running, which loses every export while install says nothing. And, with the admin's managed
// file refused (it may lock otel), install turning on the receiver or a collector anyway.
func TestTheReceiverIsOnByDefaultUnlessOptedOut(t *testing.T) {
	const refused = "the managed config file is refused"
	cases := []struct {
		name       string
		args       []string
		configured string // otel.destination already in the config
		held       string // why OTEL is held off, "" when it is not
		noHost     bool
		wantSink   bool
	}{
		{"fresh install", nil, "", "", false, true},
		{"opted out now", []string{"--otel-to", "none"}, "", "", false, false},
		{"opted out before", nil, "none", "", false, false},
		{"a collector named", []string{"--otel-to", "https://collector.example"}, "", "", false, false},
		{"no schedule to keep it up", []string{"--no-schedule"}, "", "", false, false},
		{"no service manager", nil, "", "", true, false},
		{"managed file refused", nil, "", refused, false, false},
		{"managed file refused, receiver asked for", []string{"--otel-to", "sink"}, "", refused, false, false},
		{"managed file refused, a collector named", []string{"--otel", "https://collector.example"}, "", refused, false, false},
	}
	oldHost := relayHost
	t.Cleanup(func() { relayHost = oldHost })
	for _, c := range cases {
		relayHost = func() string {
			if c.noHost {
				return ""
			}
			return "launchd"
		}
		o, err := parseInstall(c.args, c.configured, c.held)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if o.sinkMode != c.wantSink {
			t.Errorf("%s: receiver on = %v, want %v", c.name, o.sinkMode, c.wantSink)
		}
		if c.held != "" && (o.otel != "" || o.otelTo == "sink" && o.otelDefault) {
			t.Errorf("%s: OTEL to %q (default receiver %v) with the managed file refused, want none", c.name, o.otel, o.otelDefault)
		}
	}
}
