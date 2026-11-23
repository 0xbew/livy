package amp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/hook"
)

// The plugin's state lives in the data folder's amp-plugin/: state.json (when the plugin last
// loaded, and Amp's version then), and one <pid>.alive marker per running plugin, which the
// plugin writes at load and deletes on a clean shutdown (before template 3, loaded wrote it). A marker whose process is gone is a plugin that crashed
// or was killed, so events it still held may be lost: that is said once, as a ledger.error.
const (
	// StateDir is the plugin's folder in the data folder.
	StateDir  = "amp-plugin"
	stateFile = "state.json"
	// lastTurnFile holds the time of the last recorded turn.
	lastTurnFile = "last-turn"
	aliveExt     = ".alive"
	versionCmd   = "amp"
	// versionWait bounds `amp --version`, run once per plugin load, off Amp's path.
	versionWait = 5 * time.Second
	// throttleSeconds matches the Claude Code hooks' mid-session upload gap.
	throttleSeconds = "300"
	// markerTemplate is the first plugin template that writes its own marker. Written here for a
	// newer plugin, a loaded child still running at unload would put back the marker the plugin
	// had just removed, and the next load would report a clean exit as a crash.
	markerTemplate = 3
)

// DataNames are the files and folders this package keeps in the data folder.
func DataNames() []string { return []string{StateDir} }

// State is what the plugin's last load recorded.
type State struct {
	LastLoaded string `json:"last_loaded"`
	// LastTurn is when an in-scope Amp turn was last recorded, read from its own file: a plugin
	// load alone is no sign of use, since `amp plugins exec` starts a plugin process too.
	LastTurn   string `json:"-"`
	Template   string `json:"template,omitempty"`
	AmpVersion string `json:"amp_version,omitempty"`
}

// loadedEvent is what the plugin sends when it loads: its own process id and template version.
type loadedEvent struct {
	PID      int    `json:"pid"`
	Template string `json:"template"`
}

// loaded records a plugin load: dead markers become ledger.errors, this plugin's marker is
// written, and the state notes the time and Amp's version.
func loaded(stdin *os.File, stderr io.Writer) {
	dir := filepath.Join(hook.DataDir(), StateDir)
	var e loadedEvent
	obj, problem := hook.NewRecorder(stderr).Payload(stdin)
	if problem == "" {
		if err := decode(obj, &e); err != nil {
			problem = err.Error()
		}
	}
	if problem == "" && e.PID <= 0 {
		problem = "no pid"
	}
	if problem != "" {
		hook.WriteError("amp: plugin load not recorded: "+problem, stderr)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		hook.WriteError(fmt.Sprintf("amp: plugin load not recorded: %v", err), stderr)
		return
	}
	for _, pid := range DeadMarkers(dir, e.PID) {
		hook.WriteError(fmt.Sprintf("amp plugin process %d ended without a clean shutdown; events it had queued or held may be lost", pid), stderr)
		_ = os.Remove(filepath.Join(dir, strconv.Itoa(pid)+aliveExt))
	}
	now := clock.Seconds(clock.Now())
	if v, err := strconv.Atoi(e.Template); err != nil || v < markerTemplate {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(e.PID)+aliveExt), []byte(now+"\n"), 0o600); err != nil {
			hook.WriteError(fmt.Sprintf("amp: could not write the plugin's crash marker (%v); a crash of this plugin will not be detected", err), stderr)
		}
	}
	st := State{LastLoaded: now, Template: e.Template, AmpVersion: askVersion()}
	if err := writeState(filepath.Join(dir, stateFile), st); err != nil {
		hook.WriteError(fmt.Sprintf("amp: could not write %s (%v)", stateFile, err), stderr)
	}
}

// DeadMarkers is the pid of every marker in dir whose process is gone, except self.
func DeadMarkers(dir string, self int) []int {
	names, _ := filepath.Glob(filepath.Join(dir, "*"+aliveExt))
	var dead []int
	for _, n := range names {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(n), aliveExt))
		if err != nil || pid <= 0 || pid == self || alive(pid) {
			continue
		}
		dead = append(dead, pid)
	}
	return dead
}

// ReadState is the state the last plugin load wrote; ok false when there is none.
func ReadState(dataDir string) (State, bool) {
	var st State
	if t, err := os.ReadFile(filepath.Join(dataDir, StateDir, lastTurnFile)); err == nil {
		st.LastTurn = strings.TrimSpace(string(t))
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, StateDir, stateFile))
	if err != nil {
		return st, false
	}
	if json.Unmarshal(raw, &st) != nil {
		return State{LastTurn: st.LastTurn}, false
	}
	return st, true
}

func writeState(path string, st State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid()) // children run four at a time
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// versionText is what Livy keeps of `amp --version`: one short token-like line.
var versionText = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// askVersion is Amp's version as `amp --version` prints it, or "" when it cannot be had. The
// plugin API exposes no version, so it is asked once per load and kept in the state.
func askVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), versionWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, versionCmd, "--version").Output()
	if err != nil {
		return ""
	}
	for f := range strings.FieldsSeq(string(out)) {
		if versionText.MatchString(f) && strings.ContainsAny(f, "0123456789") {
			return f
		}
	}
	return ""
}

// noteTurn records the time of a recorded turn in the state, kept best-effort: the turn's lines
// are already written, and doctor only reads this to say when Amp was last recorded.
// It is a file of its own, so a turn and a plugin load never overwrite each other's fields.
func noteTurn() {
	dir := filepath.Join(hook.DataDir(), StateDir)
	path := filepath.Join(dir, lastTurnFile)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(tmp, []byte(clock.Seconds(clock.Now())+"\n"), 0o600) != nil {
		return
	}
	if os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
	}
}

// ampVersion is the version the last plugin load recorded.
func ampVersion() string {
	st, _ := ReadState(hook.DataDir())
	return st.AmpVersion
}

// startUpload starts `livy emit` in its own session, throttled unless now, and does not wait:
// it must outlive this process and the plugin, which Amp may be shutting down.
func startUpload(now bool, stderr io.Writer) {
	self, err := os.Executable()
	if err == nil {
		args := []string{"emit", "--throttle", throttleSeconds}
		if now {
			args = args[:1]
		}
		cmd := exec.Command(self, args...)
		detach(cmd)
		if err = cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return
		}
	}
	hook.WriteError(fmt.Sprintf("amp: could not start the upload (%v); the daily flush will ship it", err), stderr)
}
