package install

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The daily flush ships what a machine captured even when Claude Code is not opened again.
// macOS: a launch agent. Linux: a systemd user timer, else a crontab line. It runs
// `livy emit` with no throttle; an install never triggers an upload itself.

const (
	launchdLabel = "io.github.0xbew.livy.daily"
	systemdUnit  = "livy-daily"
	cronMarker   = "# livy daily flush, installed by `livy install`"
	// The launch agent runs at this local time. A Mac asleep then runs it on waking; a
	// StartInterval timer instead skips a firing missed in sleep and restarts at every load
	// (login, reboot, reinstall), so a laptop could go days without one.
	dailyHour   = 12
	dailyMinute = 0
)

// dailyArgs are the daily job's arguments: extract, run doctor, record its verdict as one
// doctor.status event, then sweep, so a machine that is on ships every day and says
// whether it is healthy.
func dailyArgs(home string) []string {
	return []string{"emit", "--extract", "--doctor-status", filepath.Join(home, ".livy", doctorStatusName)}
}

// Schedule records which scheduler holds the daily job, for uninstall and doctor.
type Schedule struct {
	Kind  string   `json:"kind"`  // launchd | systemd | cron | none
	Files []string `json:"files"` // what install wrote
}

func run(timeout time.Duration, stdin []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// jobEnv is the environment the job needs: schedulers start jobs with a minimal one.
func jobEnv(home string) map[string]string {
	env := map[string]string{"HOME": home,
		"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
	for _, k := range []string{"LIVY_DIR", "GOOGLE_APPLICATION_CREDENTIALS", "AWS_PROFILE", "AWS_REGION", "AWS_CONFIG_FILE"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func installSchedule(home, bin string, dry bool) (Schedule, string, bool) {
	if runtime.GOOS == "darwin" {
		return installLaunchd(home, bin, dry)
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		if _, err := run(10*time.Second, nil, "systemctl", "--user", "show-environment"); err == nil {
			return installSystemd(home, bin, dry)
		}
	}
	if _, err := exec.LookPath("crontab"); err == nil {
		return installCron(home, bin, dry)
	}
	return Schedule{Kind: "none"}, "no scheduler found (launchd, systemd --user or crontab): the daily flush is NOT installed; " +
		"capture still ships at session end and mid-session", false
}

func installLaunchd(home, bin string, dry bool) (Schedule, string, bool) {
	path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	var env strings.Builder
	for _, k := range sortedKeys(jobEnv(home)) {
		env.WriteString("\t\t<key>" + xmlEscape(k) + "</key><string>" + xmlEscape(jobEnv(home)[k]) + "</string>\n")
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array><string>` + xmlEscape(bin) + `</string>` + xmlArgs(dailyArgs(home)) + `</array>
	<key>EnvironmentVariables</key>
	<dict>
` + env.String() + `	</dict>
	<key>StartCalendarInterval</key>
	<dict><key>Hour</key><integer>` + strconv.Itoa(dailyHour) + `</integer><key>Minute</key><integer>` + strconv.Itoa(dailyMinute) + `</integer></dict>
	<key>RunAtLoad</key><false/>
	<key>ProcessType</key><string>Background</string>
</dict>
</plist>
`
	s := Schedule{Kind: "launchd", Files: []string{path}}
	if dry {
		return s, "would write " + path + " and load it with launchctl", false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Schedule{Kind: "none"}, fmt.Sprintf("could not write %s: %v", path, err), true
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return Schedule{Kind: "none"}, fmt.Sprintf("could not write %s: %v", path, err), true
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_, _ = run(20*time.Second, nil, "launchctl", "bootout", domain+"/"+launchdLabel)
	if out, err := run(20*time.Second, nil, "launchctl", "bootstrap", domain, path); err != nil {
		return s, fmt.Sprintf("wrote %s, but launchctl refused to load it: %s", path, out), true
	}
	return s, "daily flush: launch agent " + launchdLabel + " loaded", false
}

// systemdUserDir is where systemd reads user units: $XDG_CONFIG_HOME/systemd/user, else
// ~/.config/systemd/user. Install, uninstall and the relay all use it, so a unit written under
// one is found by the others.
func systemdUserDir(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "systemd", "user")
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

func installSystemd(home, bin string, dry bool) (Schedule, string, bool) {
	dir := systemdUserDir(home)
	svc, timer := filepath.Join(dir, systemdUnit+".service"), filepath.Join(dir, systemdUnit+".timer")
	var env strings.Builder
	for _, k := range sortedKeys(jobEnv(home)) {
		env.WriteString("Environment=" + systemdQuote(k+"="+jobEnv(home)[k]) + "\n")
	}
	service := "[Unit]\nDescription=Livy daily flush (" + gitHookSignature + ")\n\n[Service]\nType=oneshot\n" +
		env.String() + "ExecStart=" + systemdQuote(bin) + " " + systemdWords(dailyArgs(home)) + "\n"
	tim := "[Unit]\nDescription=Livy daily flush\n\n[Timer]\nOnCalendar=daily\nPersistent=true\nRandomizedDelaySec=1h\n\n" +
		"[Install]\nWantedBy=timers.target\n"
	s := Schedule{Kind: "systemd", Files: []string{svc, timer}}
	if dry {
		return s, "would write " + svc + " and " + timer + " and enable the timer", false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Schedule{Kind: "none"}, fmt.Sprintf("could not write %s: %v", dir, err), true
	}
	for path, body := range map[string]string{svc: service, timer: tim} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return Schedule{Kind: "none"}, fmt.Sprintf("could not write %s: %v", path, err), true
		}
	}
	_, _ = run(20*time.Second, nil, "systemctl", "--user", "daemon-reload")
	if out, err := run(20*time.Second, nil, "systemctl", "--user", "enable", "--now", systemdUnit+".timer"); err != nil {
		return s, "wrote the units, but systemctl refused to enable the timer: " + out, true
	}
	return s, "daily flush: systemd user timer " + systemdUnit + ".timer enabled", false
}

// systemdQuote quotes a unit-file word: specifiers (%) and variables ($) are taken literally.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

func systemdWords(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = systemdQuote(a)
	}
	return strings.Join(q, " ")
}

func cronLine(home, bin string) string {
	var env []string
	for _, k := range sortedKeys(jobEnv(home)) {
		env = append(env, k+"="+shellQuote(jobEnv(home)[k]))
	}
	args := dailyArgs(home)
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	line := strings.Join(env, " ") + " " + shellQuote(bin) + " " + strings.Join(args, " ") + " >/dev/null 2>&1"
	return "17 9 * * * " + strings.ReplaceAll(line, "%", `\%`) + " " + cronMarker // cron reads % as a newline
}

// readCrontab is the user's crontab; "no crontab" is empty, and any other failure is an
// error, since writing back from a failed read would replace the whole crontab.
func readCrontab() (string, error) {
	out, err := run(10*time.Second, nil, "crontab", "-l")
	if err != nil {
		if strings.HasPrefix(out, "no crontab for") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l failed: %s", out)
	}
	return out, nil
}

func installCron(home, bin string, dry bool) (Schedule, string, bool) {
	current, err := readCrontab()
	if err != nil {
		return Schedule{Kind: "none"}, "daily flush NOT installed: " + err.Error(), true
	}
	lines := withoutCron(current)
	lines = append(lines, cronLine(home, bin))
	s := Schedule{Kind: "cron"}
	if dry {
		return s, "would add a crontab line for the daily flush", false
	}
	if out, err := run(10*time.Second, []byte(strings.Join(lines, "\n")+"\n"), "crontab", "-"); err != nil {
		return Schedule{Kind: "none"}, "could not write the crontab: " + out, true
	}
	return s, "daily flush: crontab line added (09:17 every day)", false
}

// withoutCron is the crontab's lines minus ours ("no crontab" output is not a line).
func withoutCron(current string) []string {
	var kept []string
	for _, l := range strings.Split(current, "\n") {
		if l == "" || strings.HasSuffix(l, cronMarker) {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

// note is one line an uninstall step reports, and whether the step failed. The caller reads
// failed, never the wording.
type note struct {
	text   string
	failed bool
}

// removeCron takes Livy's line out of the crontab, or says it would.
func removeCron(dry bool) []note {
	current, err := readCrontab()
	if err != nil {
		return []note{{text: "crontab line not removed: " + err.Error(), failed: true}}
	}
	if !strings.Contains(current, cronMarker) {
		return nil
	}
	if dry {
		return []note{{text: "would remove the crontab line"}}
	}
	kept := withoutCron(current)
	var werr error
	if len(kept) == 0 {
		_, werr = run(10*time.Second, nil, "crontab", "-r")
	} else {
		_, werr = run(10*time.Second, []byte(strings.Join(kept, "\n")+"\n"), "crontab", "-")
	}
	if werr != nil {
		return []note{{text: "could not remove the crontab line: " + werr.Error(), failed: true}}
	}
	return []note{{text: "removed the crontab line"}}
}

func removeSchedule(s Schedule, dry bool) []note {
	var out []note
	switch s.Kind {
	case "launchd":
		if !dry {
			_, _ = run(20*time.Second, nil, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+launchdLabel)
		}
	case "systemd":
		if !dry {
			_, _ = run(20*time.Second, nil, "systemctl", "--user", "disable", "--now", systemdUnit+".timer")
		}
	case "cron":
		out = append(out, removeCron(dry)...)
	}
	for _, f := range s.Files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if dry {
			out = append(out, note{text: "would remove " + f})
		} else if err := os.Remove(f); err != nil {
			out = append(out, note{text: fmt.Sprintf("could not remove %s: %v", f, err), failed: true})
		} else {
			out = append(out, note{text: "removed " + f})
		}
	}
	if s.Kind == "systemd" && !dry {
		_, _ = run(20*time.Second, nil, "systemctl", "--user", "daemon-reload")
	}
	return out
}

func xmlArgs(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString("<string>" + xmlEscape(a) + "</string>")
	}
	return b.String()
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
