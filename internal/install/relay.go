package install

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/relay"
)

// The Codex relay (`livy relay`) runs as a KeepAlive service: a launch agent on macOS, a
// systemd user service on Linux. Codex reads its export headers once, at start, so with an
// hour's ID token it exports to the relay on loopback, which adds a fresh token to each
// export. The table names the relay only once the service has loaded, since until then
// nothing listens on its port.

const (
	relayLabel = "io.github.0xbew.livy.codex-relay"
	relayUnit  = "livy-codex-relay"
	// Not 14318: the Python recorder's relay holds that port, and keeps running through a
	// migration until its own launch agent is removed.
	RelayListen = "127.0.0.1:14319"
	// A relay that exits is started again no sooner than this, so a crash loop stays cheap.
	relayRestartSeconds = 60
)

var relayKeyInTable = regexp.MustCompile(`"` + regexp.QuoteMeta(relay.KeyHeader) + `"\s*=\s*"([^"]*)"`)

// relayHost is the scheduler that can keep the relay running: launchd, systemd, or "".
var relayHost = func() string {
	if runtime.GOOS == "darwin" {
		return "launchd"
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		if _, err := run(10*time.Second, nil, "systemctl", "--user", "show-environment"); err == nil {
			return "systemd"
		}
	}
	return ""
}

func relayArgs(collector, headersCommand string) []string {
	return []string{"relay", "--collector", strings.TrimSuffix(collector, "/"), "--listen", RelayListen,
		"--headers-command", headersCommand}
}

// relayKey is the key the relay requires, created once and then kept: Codex reads it at
// start, so a new one on every install would lock out every Codex process already running.
// A lost key file is restored from the key our table already gives Codex.
func relayKey(dir, table string) (string, error) {
	if k := relay.ReadKey(dir); k != "" {
		return k, os.Chmod(filepath.Join(dir, relay.KeyFile), 0o600)
	}
	key := ""
	if m := relayKeyInTable.FindStringSubmatch(table); m != nil && m[1] != "" && tomlSafe(m[1]) {
		key = m[1]
	} else {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		key = base64.RawURLEncoding.EncodeToString(b)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return key, writeFileAtomic(filepath.Join(dir, relay.KeyFile), []byte(key+"\n"), 0o600)
}

// loadRelay writes the relay's service and loads it. A service that does not load is
// unloaded and its files removed, so doctor never mistakes a leftover for a running relay.
var loadRelay = func(kind, home, bin string, args []string) (Schedule, error) {
	var s Schedule
	var err error
	switch kind {
	case "launchd":
		s, err = loadRelayLaunchd(home, bin, args)
	case "systemd":
		s, err = loadRelaySystemd(home, bin, args)
	default:
		return Schedule{}, errors.New("no launchd or systemd user service to keep it running")
	}
	if err != nil {
		removeRelay(s, false)
		return Schedule{}, err
	}
	return s, nil
}

func loadRelayLaunchd(home, bin string, args []string) (Schedule, error) {
	path := filepath.Join(home, "Library", "LaunchAgents", relayLabel+".plist")
	s := Schedule{Kind: "launchd", Files: []string{path}}
	var env strings.Builder
	for _, k := range sortedKeys(jobEnv(home)) {
		env.WriteString("\t\t<key>" + xmlEscape(k) + "</key><string>" + xmlEscape(jobEnv(home)[k]) + "</string>\n")
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + relayLabel + `</string>
	<key>ProgramArguments</key>
	<array><string>` + xmlEscape(bin) + `</string>` + xmlArgs(args) + `</array>
	<key>EnvironmentVariables</key>
	<dict>
` + env.String() + `	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>` + strconv.Itoa(relayRestartSeconds) + `</integer>
	<key>ProcessType</key><string>Background</string>
</dict>
</plist>
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return s, err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return s, err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_, _ = run(20*time.Second, nil, "launchctl", "bootout", domain+"/"+relayLabel)
	if out, err := run(20*time.Second, nil, "launchctl", "bootstrap", domain, path); err != nil {
		// launchctl can refuse to bootstrap a KeepAlive job it is still booting out; once more.
		time.Sleep(2 * time.Second)
		if out2, err2 := run(20*time.Second, nil, "launchctl", "bootstrap", domain, path); err2 != nil {
			return s, fmt.Errorf("launchctl refused to load it: %s", firstNonEmpty(out2, out))
		}
	}
	return s, nil
}

func loadRelaySystemd(home, bin string, args []string) (Schedule, error) {
	dir := systemdUserDir(home)
	path := filepath.Join(dir, relayUnit+".service")
	s := Schedule{Kind: "systemd", Files: []string{path}}
	var env strings.Builder
	for _, k := range sortedKeys(jobEnv(home)) {
		env.WriteString("Environment=" + systemdQuote(k+"="+jobEnv(home)[k]) + "\n")
	}
	unit := "[Unit]\nDescription=Livy Codex OTEL relay (" + gitHookSignature + ")\n\n[Service]\n" + env.String() +
		"ExecStart=" + systemdQuote(bin) + " " + systemdWords(args) + "\nRestart=always\nRestartSec=" +
		strconv.Itoa(relayRestartSeconds) + "\n\n[Install]\nWantedBy=default.target\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s, err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return s, err
	}
	_, _ = run(20*time.Second, nil, "systemctl", "--user", "daemon-reload")
	if out, err := run(20*time.Second, nil, "systemctl", "--user", "enable", relayUnit+".service"); err != nil {
		return s, fmt.Errorf("systemctl refused to enable it: %s", out)
	}
	// restart, not start: a relay already running keeps its old arguments and key otherwise.
	if out, err := run(20*time.Second, nil, "systemctl", "--user", "restart", relayUnit+".service"); err != nil {
		return s, fmt.Errorf("systemctl refused to start it: %s", out)
	}
	return s, nil
}

// removeRelay stops the relay's service and removes its files, saying what it did.
func removeRelay(s Schedule, dry bool) []note {
	var out []note
	if s.Kind == "" {
		return nil
	}
	if !dry {
		switch s.Kind {
		case "launchd":
			_, _ = run(20*time.Second, nil, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+relayLabel)
		case "systemd":
			_, _ = run(20*time.Second, nil, "systemctl", "--user", "disable", "--now", relayUnit+".service")
		}
	}
	for _, f := range s.Files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		switch {
		case dry:
			out = append(out, note{text: "would remove the Codex relay's " + f})
		case os.Remove(f) != nil:
			out = append(out, note{text: "could not remove the Codex relay's " + f, failed: true})
		default:
			out = append(out, note{text: "removed the Codex relay's " + f})
		}
	}
	if s.Kind == "systemd" && !dry {
		_, _ = run(20*time.Second, nil, "systemctl", "--user", "daemon-reload")
	}
	return out
}

// relayListening waits briefly for the relay just loaded to answer as itself. A service that
// loaded but exited at once (its port taken, its key unreadable) must not be named in the
// table, and neither may another relay already on the port with another key. So both must
// hold: the relay's state file names a process started since the load that is still alive,
// and the port answers a GET with the relay's 501 "POST only" (which it counts nowhere). A
// relay that lost the port to another writes its own pid there and exits.
var relayListening = func(since time.Time) bool {
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		st := relay.ReadState(hook.DataDir())
		started, err := time.Parse(time.RFC3339, st.StartedAt)
		if err != nil || started.Before(since.Truncate(time.Second)) || st.PID <= 0 || !alive(st.PID) {
			continue
		}
		resp, err := client.Get("http://" + RelayListen + relay.LogsPath)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotImplemented && strings.Contains(string(body), "POST only") &&
			alive(st.PID) {
			return true
		}
	}
	return false
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}
