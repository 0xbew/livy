// Package doctor is `livy doctor`: it proves capture is alive on this machine, or says
// exactly what is wrong. Hooks are silent by design, so a broken one looks exactly like a
// quiet one; the last upload is the number that tells them apart.
//
// Doctor writes and uploads nothing. The one exception: when it cannot write the status
// file it was asked for, that is a ledger.error, since the daily job's stderr reaches
// nobody. It exits 1 when capture is broken.
package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ccpolicy"
	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/githook"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/install"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/otelignore"
	"github.com/0xbew/livy/internal/relay"
	"github.com/0xbew/livy/internal/sink/local"
)

// Statuses, worst last.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
)

// StaleAfter is how long captured data may wait unshipped before it is broken, not quiet:
// the daily flush alone keeps a machine that is on under a day.
var StaleAfter = 48 * time.Hour

// Row is one check. Detail is for the person reading; it never leaves the machine.
type Row struct {
	Check  string `json:"check"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report is every check and the verdict.
type Report struct {
	OK   bool  `json:"ok"`
	Rows []Row `json:"checks"`
}

func (r *Report) add(status, check, format string, a ...any) {
	r.Rows = append(r.Rows, Row{Check: check, Status: status, Detail: fmt.Sprintf(format, a...)})
}

// commandTimeout bounds each external command a check runs.
const commandTimeout = 10 * time.Second

// probeTimeout is the backstop on the OTLP probe's client; the probe's own deadline is shorter.
const probeTimeout = 60 * time.Second

// output runs a command under commandTimeout, or sooner if ctx ends, and returns its stdout.
func output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	return exec.CommandContext(cctx, name, args...).Output()
}

// Check runs every check. sinkFor is how emit resolves the sink.
func Check(ctx context.Context, sinkFor func() (emit.Sink, error)) Report {
	var r Report
	dir := hook.DataDir()
	res := hook.Settings()

	// the binary and its own selftest: runtime rows, like Python's interpreter and selftests
	self, _ := os.Executable()
	r.add(OK, "binary", "livy at %s", self)
	var selftestOut bytes.Buffer
	if hook.Selftest(&selftestOut, &selftestOut) == 0 {
		r.add(OK, "selftest", "selftest ok: events written, refused and scoped as expected")
	} else {
		r.add(Fail, "selftest", "selftest FAILED: %s", strings.TrimSpace(selftestOut.String()))
	}

	checkConfig(&r, res)
	err := checkSink(ctx, &r, sinkFor, dir)

	checkClaudeVersions(ctx, &r, hook.Home())
	managed := readManaged(&r, ccpolicy.Dir())
	checkHooks(&r, managed)
	checkSchedule(ctx, &r)
	checkRepo(ctx, &r)
	checkHookRedirects(&r)
	checkTrailers(&r)
	checkGitHooks(&r)
	lastEmitError := checkLedger(&r, dir)
	if errors.Is(err, emit.ErrNoSink) {
		// Nothing can ship, so data waiting past StaleAfter is the configuration, not a stuck upload.
		r.add(Warn, "last upload", "none: no sink is configured, so nothing ships")
	} else {
		checkLastUpload(&r, dir, lastEmitError)
	}
	held := otelHeld(&r, res)
	if !held {
		checkOtel(ctx, &r, managed)
		checkCodexOtel(ctx, &r)
	}
	checkCodexHooks(&r)
	checkAmp(ctx, &r)
	if !held {
		checkOtelReceiver(ctx, &r)
		checkOtelIgnore(&r, res)
	}
	checkDenylist(&r)
	checkPrompts(&r, dir)

	r.OK = true
	for _, row := range r.Rows {
		if row.Status == Fail {
			r.OK = false
		}
	}
	return r
}

// checkConfig adds the config, data folder, scope and identity rows.
func checkConfig(r *Report, res config.Resolved) {
	if len(res.Refused) > 0 {
		for _, p := range res.Refused {
			r.add(Fail, "config", "%s", p.String())
		}
	} else {
		r.add(OK, "config", "user %s; managed %s", hook.UserConfigPath(), hook.ManagedPath())
	}
	for _, o := range res.Locked {
		r.add(Warn, "config", "%s", o.String())
	}
	// No row while every data folder is private: one row per folder that was not.
	for _, n := range install.FixPrivate() {
		status := Warn
		if n.Failed {
			status = Fail
		}
		r.add(status, "data folder", "%s", n.Text)
	}
	checkScope(r, res.ManagedUnusable)
	if emit.Handle() == "" {
		r.add(Warn, "identity", "no identity.handle: objects carry no handle= part")
	}
}

// tilde is path with the home folder written as ~.
func tilde(path string) string {
	home := hook.Home()
	if rel, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return path
}

// checkSink adds the sink row: configured, and its credentials answer, without writing. It
// returns how the sink resolved.
func checkSink(ctx context.Context, r *Report, sinkFor func() (emit.Sink, error), dir string) error {
	sink, err := sinkFor()
	switch {
	case errors.Is(err, emit.ErrNoSink):
		r.add(Warn, "sink", "not configured: captured data stays in %s until sink.type is set", dir)
	case err != nil:
		r.add(Fail, "sink", "%v", err)
	default:
		where := strings.TrimSuffix(sink.URL("x"), "x")
		if l, ok := sink.(*local.Sink); ok {
			where = "local · " + tilde(l.Root)
		}
		if ready, ok := sink.(interface{ Ready(context.Context) error }); ok {
			cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err := ready.Ready(cctx)
			cancel()
			if err != nil {
				r.add(Fail, "sink", "%s: credentials or destination not usable: %v", where, err)
				break
			}
		}
		r.add(OK, "sink", "%s", where)
	}
	return err
}

// required is, per Claude Code event, the Livy command capture depends on there. The
// throttled upload sharing some of these events does not stand in for it.
var required = map[string]string{"SessionStart": "hook session.started", "UserPromptSubmit": "hook prompt.submitted",
	"PostToolUse": "hook work.pr_opened", "PostModelSwitch": "hook model.switched",
	"InstructionsLoaded": "hook instructions.loaded", "Stop": "hook turn.ended", "SessionEnd": "--event session.ended"}

// mustMatch is what a group's matcher must let through for Livy's hook there to record
// anything: PR opens come from Bash calls, sessions from a fresh start.
var mustMatch = map[string]string{"PostToolUse": "Bash", "SessionStart": "startup"}

// matches is Claude Code's matcher rule: empty or "*" matches all, else a regex on the
// whole subject.
func matches(matcher, subject string) bool {
	if matcher == "" || matcher == "*" || subject == "" {
		return true
	}
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && re.MatchString(subject)
}

// hookGroup is one matcher group of a Claude Code hook event, as far as doctor reads it.
type hookGroup struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	} `json:"hooks"`
}

// livyHooks is the required events with no Livy hook that would run, in name order, and the
// Livy binaries the hooks that would run call. A hook runs required[ev] when its command
// ends with it, or ends it with `;` and goes on (Codex's SessionEnd starts the upload after).
func livyHooks(groups map[string][]hookGroup, required, mustMatch map[string]string) ([]string, map[string]bool) {
	var missing []string
	bins := map[string]bool{}
	events := make([]string, 0, len(required))
	for ev := range required {
		events = append(events, ev)
	}
	sort.Strings(events)
	for _, ev := range events {
		found := false
		for _, g := range groups[ev] {
			if !matches(g.Matcher, mustMatch[ev]) {
				continue // a group whose matcher excludes what Livy records there never runs for it
			}
			for _, h := range g.Hooks {
				if bin := install.OurBinary(h.Command); bin != "" && h.Type == "command" && strings.Contains(h.Command+";", " "+required[ev]+";") {
					found = true
					bins[bin] = true
				}
			}
		}
		if !found {
			missing = append(missing, ev)
		}
	}
	return missing, bins
}

func checkHooks(r *Report, managed ccpolicy.Settings) {
	st, settingsPath := install.Recorded()
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		r.add(Fail, "hooks", "%s unreadable (%v): no Claude Code hook runs Livy; run `livy install`", settingsPath, err)
		return
	}
	var doc struct {
		Hooks   map[string][]hookGroup `json:"hooks"`
		NoHooks bool                   `json:"disableAllHooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.add(Fail, "hooks", "%s is not the settings shape Livy reads (%v)", settingsPath, err)
		return
	}
	checkManagedHooks(r, managed, doc.NoHooks)
	missing, bins := livyHooks(doc.Hooks, required, mustMatch)
	if len(missing) > 0 {
		r.add(Fail, "hooks", "no Livy hook for %s in %s; run `livy install`", strings.Join(missing, ", "), settingsPath)
		return
	}
	for bin := range bins {
		if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
			r.add(Fail, "hooks", "the hooks run %s, which is missing or not executable", bin)
			return
		}
	}
	_ = st
	r.add(OK, "hooks", "all %d events in %s", len(required), settingsPath)
}

func checkSchedule(ctx context.Context, r *Report) {
	st, _ := install.Recorded()
	switch st.Schedule.Kind {
	case "launchd", "systemd":
		for _, f := range st.Schedule.Files {
			if _, err := os.Stat(f); err != nil {
				r.add(Fail, "daily flush", "%s is missing: the daily flush is not installed", f)
				return
			}
		}
		cmds := [][]string{{"launchctl", "print", "gui/" + strconv.Itoa(os.Getuid()) + "/io.github.0xbew.livy.daily"}}
		if st.Schedule.Kind == "systemd" {
			// Enabled is not enough: a stopped timer runs nothing until the next login.
			cmds = [][]string{{"systemctl", "--user", "is-enabled", "livy-daily.timer"},
				{"systemctl", "--user", "is-active", "livy-daily.timer"}}
		}
		for _, cmd := range cmds {
			cctx, cancel := context.WithTimeout(ctx, commandTimeout)
			err := exec.CommandContext(cctx, cmd[0], cmd[1:]...).Run()
			cancel()
			if err != nil {
				r.add(Fail, "daily flush", "%s job is written but not running (%s: %v); run `livy install` again", st.Schedule.Kind, strings.Join(cmd, " "), err)
				return
			}
		}
		r.add(OK, "daily flush", "%s", st.Schedule.Kind)
	case "cron":
		out, err := output(ctx, "crontab", "-l")
		if err != nil || !bytes.Contains(out, []byte("livy daily flush")) {
			r.add(Fail, "daily flush", "the crontab line is missing")
			return
		}
		r.add(OK, "daily flush", "crontab")
	default:
		r.add(Warn, "daily flush", "not installed: a machine that stops opening Claude Code ships nothing")
	}
}

// checkScope names the orgs and the repos in scope; with neither, nothing is recorded.
func checkScope(r *Report, managedUnusable bool) {
	orgs, repos := hook.Orgs(io.Discard), hook.Repos(io.Discard)
	switch {
	case len(orgs) == 0 && len(repos) == 0 && managedUnusable:
		r.add(Fail, "scope", "no org in scope while the managed file is refused: nothing is recorded anywhere")
	case len(orgs) == 0 && len(repos) == 0:
		r.add(Fail, "scope", "no org configured (scope.orgs): nothing is recorded anywhere")
	case len(repos) == 0:
		r.add(OK, "scope", "orgs %s", strings.Join(orgs, ", "))
	case len(orgs) == 0:
		r.add(OK, "scope", "repos %s", strings.Join(repos, ", "))
	default:
		r.add(OK, "scope", "orgs %s; repos %s", strings.Join(orgs, ", "), strings.Join(repos, ", "))
	}
}

// checkRepo says whether the folder doctor runs in is in scope and has the git hook: the
// one place an out-of-scope repo's silence is explained.
func checkRepo(ctx context.Context, r *Report) {
	g := gitinfo.New(io.Discard)
	origin := g.Origin("")
	if origin == "" {
		r.add(OK, "this repo", "not a repository with an origin: nothing here is recorded")
		return
	}
	if !g.InScope(origin, func() []string { return hook.Scope(io.Discard) }) {
		r.add(OK, "this repo", "%s is out of scope: nothing here is recorded, by design", g.Slug())
		return
	}
	path, err := output(ctx, "git", "rev-parse", "--path-format=absolute", "--git-path", "hooks/post-checkout")
	body, rerr := os.ReadFile(strings.TrimSpace(string(path)))
	if err != nil || rerr != nil || !bytes.Contains(body, []byte("installed by `livy install`")) {
		r.add(Warn, "this repo", "%s is in scope, but has no Livy post-checkout: branch switches are not recorded; run `livy install` in it", g.Slug())
		return
	}
	r.add(OK, "this repo", "%s is in scope, git hook in place", g.Slug())
}

// checkHookRedirects warns about a repo whose tracked core.hooksPath Livy routed through
// wrappers when something since install has stopped the wrappers covering it. No row when
// nothing is wrong or no repo is redirected.
func checkHookRedirects(r *Report) {
	st, _ := install.Recorded()
	if problems := install.RedirectProblems(st); len(problems) > 0 {
		r.add(Warn, "git hooks redirect", "%s", strings.Join(problems, "; "))
	}
}

// trailerFix is the command that stamps the repos behind missing, and what no install by repo
// can fix: a shared hooks folder, or a repo now out of scope.
func trailerFix(missing []string, bin string) string {
	var parts, foreign []string
	var fixable, not []string
	for _, d := range missing {
		ok, no := install.Revisited([]string{d})
		switch {
		case len(ok) > 0 && install.ForeignTrailerHook(d):
			foreign = append(foreign, d)
		default:
			fixable, not = append(fixable, ok...), append(not, no...)
		}
	}
	if len(foreign) > 0 {
		parts = append(parts, "another tool's prepare-commit-msg in "+strings.Join(foreign, ", ")+
			": add `"+install.ShellQuote(bin)+" git prepare-commit-msg \"$@\"` to it, before any exit (`livy install` leaves it alone)")
	}
	if len(fixable) > 0 {
		cmd := []string{"livy install"}
		for _, repo := range fixable {
			cmd = append(cmd, "--repo", install.ShellQuote(repo))
		}
		parts = append(parts, "run `"+strings.Join(cmd, " ")+"`")
	}
	var dropped, shared []string
	for _, d := range not {
		if repo := install.RepoOfHooks(d); install.IsRepo(repo) {
			dropped = append(dropped, "--repo", install.ShellQuote(repo))
		} else {
			shared = append(shared, d)
		}
	}
	if len(dropped) > 0 {
		parts = append(parts, "a repo out of scope: `livy uninstall "+strings.Join(dropped, " ")+"` takes Livy's hooks out")
	}
	if len(shared) > 0 {
		parts = append(parts, strings.Join(shared, ", ")+" is a shared hooks folder no in-scope repo owns: run "+
			"`livy install --repo <repo>` with a repo that uses it")
	}
	return strings.Join(parts, "; ")
}

// checkGitHooks warns when a hook install wrote is gone from a repo it hooked, with the command
// that puts it back. No row when every recorded hook is in place.
func checkGitHooks(r *Report) {
	st, _ := install.Recorded()
	missing, gone := install.HookGaps(st)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("Livy's git hooks are gone from %d repo(s): %s", len(missing), trailerFix(missing, st.Binary)))
	}
	if len(gone) > 0 {
		parts = append(parts, "no longer on disk: "+strings.Join(gone, ", ")+" (the next `livy install` drops them from the record)")
	}
	if len(parts) > 0 {
		r.add(Warn, "git hooks", "%s", strings.Join(parts, "; "))
	}
}

// checkTrailers says, for the repos install put Livy's git hooks in, whether agent commits
// there get attribution trailers. No row when install recorded no repo.
func checkTrailers(r *Report) {
	st, _ := install.Recorded()
	repos, missing := install.TrailerCoverage(st)
	switch {
	case repos == 0:
	case !githook.TrailersEnabled():
		r.add(OK, "commit trailers", "off (trailers.enabled is false)")
	case len(missing) > 0:
		r.add(Warn, "commit trailers", "agent commits are not stamped in %d of %d repo(s): %s; %s",
			len(missing), repos, strings.Join(missing, ", "), trailerFix(missing, st.Binary))
	default:
		r.add(OK, "commit trailers", "agent commits are stamped in all %d repo(s) with Livy's git hooks", repos)
	}
}

// uploadFailed says an emit ledger.error means a pass shipped nothing it should have:
// the sink refused, no sink, no lock, no rotation. Notes from a pass that did ship (a
// segment with an unparseable line, one it could not delete) are not failures.
func uploadFailed(reason string) bool {
	if !strings.HasPrefix(reason, "emit: ") {
		return false
	}
	for _, s := range []string{"not uploaded", "nothing uploaded", "cannot open", "could not rotate", "ledger lock not acquired"} {
		if strings.Contains(reason, s) {
			return true
		}
	}
	return false
}

// emitError is the newest ledger.error emit wrote: an upload pass that failed.
type emitError struct {
	at     time.Time
	reason string
}

func checkLedger(r *Report, dir string) (last emitError) {
	path := filepath.Join(dir, ledger.FileName)
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add(OK, "ledger", "empty (everything captured so far has shipped, or nothing has happened)")
	case err != nil:
		r.add(Fail, "ledger", "%s unreadable: %v", path, err)
	default:
		last = ledgerRow(r, path, f)
	}
	if n := countSegments(r, dir, emit.RejectedDir, "rejected segments"); n > 0 {
		r.add(Fail, "rejected segments", "%d in %s: never uploaded", n, filepath.Join(dir, emit.RejectedDir))
	}
	if n := countSegments(r, dir, emit.SegmentsDir, "pending segments"); n > 0 {
		r.add(Warn, "pending segments", "%d waiting to upload", n)
	}
	return last
}

// ledgerRow reads the live ledger and adds its row; it returns the newest failed upload in it.
func ledgerRow(r *Report, path string, f *os.File) (last emitError) {
	total, torn, errs := 0, 0, 0
	newest := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		total++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			torn++ // a record the writer must never produce
			continue
		}
		var rec struct {
			Event  string `json:"event"`
			TS     string `json:"ts"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			torn++
			continue
		}
		newest = rec.TS
		t, err := time.Parse(time.RFC3339, rec.TS)
		if rec.Event == "ledger.error" {
			errs++
		}
		if err == nil && rec.Event == "ledger.error" && uploadFailed(rec.Reason) && !t.Before(last.at) {
			last = emitError{t, strings.TrimPrefix(rec.Reason, "emit: ")}
		}
	}
	_ = f.Close()
	switch {
	case sc.Err() != nil:
		r.add(Fail, "ledger", "%s could not be read past line %d (%v): what follows is unchecked", path, total, sc.Err())
	case torn > 0:
		r.add(Fail, "ledger", "%d line(s), %d UNREADABLE: capture wrote garbage", total, torn)
	case errs > 0:
		r.add(Fail, "ledger", "%d line(s), newest %s; %d ledger.error — read them in %s", total, newest, errs, path)
	default:
		r.add(OK, "ledger", "%d line(s) waiting, newest %s", total, newest)
	}
	return last
}

// countSegments is how many segments a folder below dir holds. A folder that cannot be read
// is said, never counted as empty: what is in it may be data that never shipped.
func countSegments(r *Report, dir, sub, row string) int {
	n := 0
	var unread []string
	_ = filepath.WalkDir(filepath.Join(dir, sub), func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && errors.Is(err, fs.ErrNotExist) && p == filepath.Join(dir, sub):
		case err != nil:
			unread = append(unread, fmt.Sprintf("%s (%v)", p, err))
		case !d.IsDir() && strings.HasSuffix(p, ".jsonl"):
			n++
		}
		return nil
	})
	if len(unread) > 0 {
		r.add(Fail, row, "cannot read %s: what it holds may never have shipped", strings.Join(unread, ", "))
	}
	return n
}

func checkLastUpload(r *Report, dir string, lastErr emitError) {
	path := filepath.Join(dir, emit.LastUploadKey)
	raw, err := os.ReadFile(path)
	oldest, captured, unread := oldestWaiting(dir)
	if unread {
		r.add(Fail, "last upload", "a folder holding waiting data cannot be read: how long it has waited is UNKNOWN")
	}
	var last emit.LastUpload
	var at time.Time
	detail := "never"
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err == nil && json.Unmarshal(raw, &last) == nil:
		if at, err = time.Parse(time.RFC3339, last.TS); err != nil {
			r.add(Fail, "last upload", "%s holds no readable time: freshness UNKNOWN", path)
			return
		}
		detail = fmt.Sprintf("%s (%.1fh ago) -> %s", last.TS, clock.Now().Sub(at).Hours(), last.Object)
	default:
		r.add(Fail, "last upload", "%s exists but cannot be read: upload state UNKNOWN, not clean", path)
		return
	}
	status, why := UploadVerdict(clock.Now(), at, lastErr.at, oldest, captured)
	if why == WhyFailed {
		reason := lastErr.reason
		if len(reason) > 300 {
			reason = reason[:300] + "..."
		}
		why = fmt.Sprintf("the last attempt (%s) FAILED: %s", clock.Seconds(lastErr.at), reason)
	}
	if why != "" {
		detail += "; " + why
	}
	r.add(status, "last upload", "%s", detail)
}

// Why an upload row is not ok.
const (
	WhyFailed  = "the last attempt failed"
	WhyStale   = "STALE: capture is not reaching the sink"
	WhyNever   = "data is captured here but nothing has reached the sink"
	WhyNothing = "nothing captured yet"
	WhyPending = "captured data has not shipped yet"
)

// UploadVerdict is the last-upload row, the number that tells quiet from broken. last is
// the last confirmed upload and lastFailure the last failed attempt; oldest is when the
// oldest data still waiting locally was captured (each zero: none). A failed attempt newer
// than the last upload is broken; data that has waited longer than StaleAfter is broken;
// a quiet machine, however long since its last upload, is not.
func UploadVerdict(now, last, lastFailure, oldest time.Time, captured bool) (status, why string) {
	switch {
	case !lastFailure.IsZero() && lastFailure.After(last):
		return Fail, WhyFailed
	case !captured && last.IsZero():
		return Warn, WhyNothing
	case !captured:
		return OK, ""
	case now.Sub(oldest) > StaleAfter && last.IsZero():
		return Fail, WhyNever
	case now.Sub(oldest) > StaleAfter:
		return Fail, WhyStale
	case last.IsZero():
		return Warn, WhyPending
	}
	return OK, ""
}

// Claude Code settings that change what its export carries or where it goes. Install never
// sets one, so one that is on was decided elsewhere.
var (
	// Prompt, reply or tool text in the export, or detailed beta tracing, which sends past
	// the collector to its own endpoint.
	otelContentFlags = []string{"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_TOOL_DETAILS",
		"OTEL_LOG_TOOL_CONTENT", "OTEL_LOG_RAW_API_BODIES", "ENABLE_BETA_TRACING_DETAILED", "BETA_TRACING_ENDPOINT"}
	// A per-signal endpoint overrides the base one, so that signal skips the probed collector.
	otelSignalEndpoints = []string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"}
	// A per-signal protocol overrides the base one; the header helper works over HTTP only.
	otelSignalProtocols = []string{"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"}
)

// flagOn is how a person setting a flag means it: anything but blank, 0, false, no, off, none.
func flagOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off", "none":
		return false
	}
	return true
}

// OtelSettingsProblems are the rows a configured export fails on before any probe: content
// capture on, a signal sent to another endpoint, or a signal off OTLP/HTTP. Claude Code
// applies the settings env over the process env, so the value it would see is judged.
func OtelSettingsProblems(settingsEnv map[string]any, getenv func(string) string) []Row {
	// Claude Code puts settings env values into the process env as text, so `true` or 1 count.
	str := func(k string) (string, bool) {
		v, ok := settingsEnv[k]
		if !ok || v == nil {
			return "", ok
		}
		if s, isStr := v.(string); isStr {
			return s, true
		}
		return fmt.Sprint(v), true
	}
	val := func(k string) string {
		if s, ok := str(k); ok {
			return s
		}
		return getenv(k)
	}
	effective := func(names []string, bad func(string) bool) (on []string) {
		for _, k := range names {
			if bad(val(k)) {
				on = append(on, k)
			}
		}
		return on
	}
	var rows []Row
	if on := effective(otelContentFlags, flagOn); len(on) > 0 {
		rows = append(rows, Row{Check: "otel content", Status: Fail, Detail: "content capture is ON (" +
			strings.Join(on, ", ") + "): prompt, reply or tool text would be sent, or sent past the collector; unset it unless that is deliberate"})
	}
	if on := effective(otelSignalEndpoints, flagOn); len(on) > 0 {
		rows = append(rows, Row{Check: "otel endpoint", Status: Fail, Detail: strings.Join(on, ", ") +
			" set: that signal goes to its own endpoint, not the installed collector; unset it"})
	}
	var off []string
	for _, k := range otelSignalProtocols {
		if v := val(k); v != "" && v != "http/protobuf" && v != "http/json" {
			off = append(off, k)
		}
	}
	if len(off) > 0 {
		rows = append(rows, Row{Check: "otel protocol", Status: Fail, Detail: strings.Join(off, ", ") +
			" set to something other than http/protobuf or http/json: that signal will not reach the collector; unset it"})
	}
	return rows
}

// OtelProbeVerdict judges an empty export's answer. Only a 2xx means the collector takes what
// this machine sends: a refusal (401, 403) is export that never arrives, whether or not a
// header was sent. err is a probe that got no answer, which reads as offline, not broken.
func OtelProbeVerdict(status int, err error) (string, string) {
	switch {
	case err != nil:
		return Warn, "does not answer from here (offline or VPN?): " + err.Error()
	case status >= 200 && status < 300:
		return OK, fmt.Sprintf("accepted an empty export (HTTP %d)", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return Fail, fmt.Sprintf("refused an empty export (HTTP %d): telemetry is not arriving", status)
	default:
		return Fail, fmt.Sprintf("answered an empty export with HTTP %d: telemetry is not arriving", status)
	}
}

// otelHeld adds the otel row for a refused managed file, which may lock otel, and is whether
// it did: install then sets up no export and the receiver refuses each one, so nothing is probed.
func otelHeld(r *Report, res config.Resolved) bool {
	if !res.ManagedUnusable {
		return false
	}
	r.add(Fail, "otel", "held off while the managed config file is refused, as it may lock otel: livy install sets up "+
		"no OTEL export and removes its own, and Livy's receiver forwards and stores nothing; fix the managed file, then re-run livy install")
	return true
}

// checkOtel: OTEL export is optional. Configured, it must be content-free and accepted: the
// header helper is run, as Claude Code would run it, and its headers go on an empty export,
// which proves auth and reachability without writing anything a collector would store.
func checkOtel(ctx context.Context, r *Report, managed ccpolicy.Settings) {
	st, settingsPath := install.Recorded()
	raw, _ := os.ReadFile(settingsPath)
	var doc struct {
		Env    map[string]any `json:"env"`
		Helper string         `json:"otelHeadersHelper"`
	}
	_ = json.Unmarshal(raw, &doc)
	endpoint, _ := doc.Env["OTEL_EXPORTER_OTLP_ENDPOINT"].(string)
	on, _ := doc.Env["CLAUDE_CODE_ENABLE_TELEMETRY"].(string)
	if endpoint == "" || on != "1" {
		r.add(OK, "otel", "not configured (optional)")
		return
	}
	if checkManagedOtel(r, managed, st) {
		return
	}
	r.Rows = append(r.Rows, OtelSettingsProblems(effectiveEnv(doc.Env, managed), os.Getenv)...)
	// The probe carries a credential, so it goes only to the endpoint install set, and only
	// install's own helper is run: a value someone else wrote is reported, never executed.
	if installed := st.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]; installed == "" || installed != endpoint || doc.Helper != st.HeadersHelper {
		r.add(Fail, "otel", "the settings no longer match what livy install set (endpoint %s, otelHeadersHelper); not probed: re-run livy install --otel", endpoint)
		return
	}
	headers := map[string]string{}
	if doc.Helper != "" {
		h, err := runHeadersHelper(ctx, doc.Helper, endpoint, doc.Env)
		if err != nil {
			r.add(Fail, "otel", "the header helper produced no header: %v", err)
			return
		}
		headers = h
	}
	status, err := otlpProbe(ctx, endpoint, "/v1/logs", `{"resourceLogs":[]}`, headers)
	verdict, why := OtelProbeVerdict(status, err)
	if verdict == Fail && doc.Helper != "" && len(headers) == 0 && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		// An env-variable helper prints {} where its variable is unset, as in a scheduled job.
		verdict, why = Warn, why+"; the header helper printed no header here (is its variable set only where Claude Code runs?)"
	}
	r.add(verdict, "otel", "%s %s; %s", endpoint, why, claudeSignals(doc.Env))
	if verdict != OK {
		return
	}
	// A collector with no pipeline for a signal answers 404 there, and the harnesses drop it.
	for _, sig := range []struct{ key, row, path, body string }{
		{"OTEL_TRACES_EXPORTER", "otel traces", "/v1/traces", `{"resourceSpans":[]}`},
		{"OTEL_METRICS_EXPORTER", "otel metrics", "/v1/metrics", `{"resourceMetrics":[]}`},
	} {
		if st.Env[sig.key] != "otlp" {
			continue
		}
		status, err = otlpProbe(ctx, endpoint, sig.path, sig.body, headers)
		verdict, why = OtelProbeVerdict(status, err)
		r.add(verdict, sig.row, "%s %s", endpoint, why)
	}
}

// claudeSignals names the OTEL signals Claude Code's settings turn on and off.
func claudeSignals(env map[string]any) string {
	var on, off []string
	for _, sig := range []string{"logs", "metrics", "traces"} {
		if v, _ := env["OTEL_"+strings.ToUpper(sig)+"_EXPORTER"].(string); v != "" && v != "none" {
			on = append(on, sig)
		} else {
			off = append(off, sig)
		}
	}
	if len(on) == 0 {
		return "signals on: none"
	}
	out := "signals on: " + strings.Join(on, ", ")
	if len(off) > 0 {
		out += "; off: " + strings.Join(off, ", ")
	}
	return out
}

// runHeadersHelper runs the settings' otelHeadersHelper as Claude Code does, with the
// installed endpoint in its environment, and reads the JSON object of headers it prints.
func runHeadersHelper(ctx context.Context, helper, endpoint string, settingsEnv map[string]any) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", helper)
	// Claude Code gives the helper its settings env over the process env.
	cmd.Env = os.Environ()
	for k, v := range settingsEnv {
		if v != nil {
			cmd.Env = append(cmd.Env, k+"="+fmt.Sprint(v))
		}
	}
	cmd.Env = append(cmd.Env, "OTEL_EXPORTER_OTLP_ENDPOINT="+endpoint)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	var h map[string]string
	if err == nil && len(bytes.TrimSpace(out)) > 0 && json.Unmarshal(out, &h) != nil {
		return nil, errors.New("it printed something other than a JSON object of header strings")
	}
	if err != nil || len(bytes.TrimSpace(out)) == 0 {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		why := lines[len(lines)-1]
		if why == "" && err != nil {
			why = err.Error()
		}
		if why == "" {
			why = "no output"
		}
		return nil, errors.New(why)
	}
	return h, nil
}

// otlpProbe POSTs an empty OTLP export and returns the HTTP status.
func otlpProbe(ctx context.Context, endpoint, path, body string, headers map[string]string) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+path,
		strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	// A redirect is not followed: it would carry the bearer token on, even from https to
	// http on the same host. A 3xx is then judged like any answer that is not a 2xx.
	client := &http.Client{Timeout: probeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// checkCodexOtel says when Codex's export carries a stored header: Codex reads headers
// only literally, so install wrote the value into its config (mode 0600).
func checkCodexOtel(ctx context.Context, r *Report) {
	st, _ := install.Recorded()
	if st.CodexConfig == "" {
		return
	}
	raw, err := os.ReadFile(st.CodexConfig)
	if err != nil || !bytes.Contains(raw, []byte("`livy install` owns the [otel] table")) {
		r.add(Warn, "codex otel", "the [otel] table install wrote is not in %s", st.CodexConfig)
		return
	}
	if st.CodexRelay.Kind != "" {
		checkCodexRelay(ctx, r, st, raw)
		return
	}
	var on []string
	for _, kv := range [][2]string{{"logs", "\nexporter = {"}, {"metrics", "\nmetrics_exporter = {"}, {"traces", "\ntrace_exporter = {"}} {
		if bytes.Contains(raw, []byte(kv[1])) {
			on = append(on, kv[0])
		}
	}
	exporting := "exporting " + strings.Join(on, ", ")
	if len(on) == 0 {
		exporting = "every signal off"
	}
	if !bytes.Contains(raw, []byte(`"Authorization"`)) {
		r.add(OK, "codex otel", "%s, no stored header", exporting)
		return
	}
	if fi, err := os.Stat(st.CodexConfig); err == nil && fi.Mode().Perm()&0o077 != 0 {
		r.add(Fail, "codex otel", "%s holds an Authorization header and is readable by others (%o); chmod 600 it", st.CodexConfig, fi.Mode().Perm())
		return
	}
	r.add(Warn, "codex otel", "%s holds an Authorization header (mode 0600): Codex cannot read one from the environment", st.CodexConfig)
}

// checkOtelReceiver: with --otel-to sink, Livy's receiver must be up, and what it stored must
// be shipping. An export made while it is down is lost (Claude Code and Codex keep no queue
// across a refused connection), so a receiver that is not answering fails.
func checkOtelReceiver(ctx context.Context, r *Report) {
	st, _ := install.Recorded()
	if st.OtelDestination != "sink" {
		return
	}
	dir := hook.DataDir()
	key := relay.ReadKey(dir)
	if key == "" {
		r.add(Fail, "otel receiver", "no key in %s: the receiver refuses every export; re-run livy install --otel-to sink",
			filepath.Join(dir, relay.KeyFile))
		return
	}
	state := relay.ReadState(dir)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := otlpProbe(pctx, "http://"+install.RelayListen, "/v1/logs", `{"resourceLogs":[]}`,
		map[string]string{relay.KeyHeader: key})
	switch {
	case err != nil:
		r.add(Fail, "otel receiver", "Livy's OTEL receiver on %s is not answering (%v): every Claude Code and Codex export is lost until it is; its %s service should restart it, else re-run livy install --otel-to sink",
			install.RelayListen, err, st.CodexRelay.Kind)
		return
	case status != 200:
		r.add(Fail, "otel receiver", "Livy's OTEL receiver on %s answered an empty export with HTTP %d", install.RelayListen, status)
		return
	}
	last := state.LastDeliveredAt
	if last == "" {
		last = "nothing yet"
	}
	r.add(OK, "otel receiver", "up on %s (pid %d since %s); last export stored: %s; %d stored since %s",
		install.RelayListen, state.PID, state.StartedAt, last, state.Delivered, firstNonEmpty(state.CountingSince, state.StartedAt))
	now := clock.Now()
	if t, err := time.Parse(time.RFC3339, state.LastDroppedAt); err == nil && now.Sub(t) < 24*time.Hour {
		r.add(Warn, "otel receiver", "%d export(s) dropped by the stream's size cap, the last at %s: the otel stream is not shipping",
			state.Dropped, state.LastDroppedAt)
	}
	if t, err := time.Parse(time.RFC3339, state.LastFailedAt); err == nil && now.Sub(t) < 24*time.Hour {
		r.add(Warn, "otel receiver", "%d export(s) not stored since %s, the last at %s: %s",
			state.Failed, firstNonEmpty(state.CountingSince, state.StartedAt), state.LastFailedAt, state.LastFailure)
	}
	od := relay.OtelStreamDir(dir)
	if oldest, waiting, _ := oldestWaiting(od); waiting {
		age := now.Sub(oldest).Round(time.Minute)
		lvl := OK
		if age > 48*time.Hour {
			lvl = Fail
		} else if age > 24*time.Hour {
			lvl = Warn
		}
		r.add(lvl, "otel queue", "OTEL data waiting to ship since %s (%s); the next emit ships it", oldest.UTC().Format(time.RFC3339), age)
	} else {
		r.add(OK, "otel queue", "nothing waiting to ship")
	}
}

// checkOtelIgnore shows the otel.ignore list and what the receiver dropped for it. Only Livy's
// own receiver applies it: a collector gets Claude Code's exports straight from Claude Code.
func checkOtelIgnore(r *Report, res config.Resolved) {
	l := otelignore.FromSettings(res, hook.Home())
	if l.Empty() {
		r.add(OK, "otel ignore", "no project listed (%s): every project's OTEL is kept", otelignore.Key)
		return
	}
	listed := fmt.Sprintf("%d listed by the %s config (%s)", len(l.Entries), l.Layer, strings.Join(l.Entries, ", "))
	if len(l.Invalid) > 0 {
		r.add(Warn, "otel ignore", "%s match nothing (an entry is a folder, /path or ~/path, or owner/repo); the list "+
			"still drops records naming no project",
			strings.Join(l.Invalid, ", "))
	}
	st, _ := install.Recorded()
	switch st.OtelDestination {
	case "sink":
	case "", "none":
		r.add(OK, "otel ignore", "%s; OTEL is off, so nothing is exported", listed)
		return
	default:
		r.add(Warn, "otel ignore", "%s, but OTEL goes to %s, not Livy's receiver: Claude Code exports to it directly, so the list is "+
			"not applied; livy install --otel-to sink applies it", listed, st.OtelDestination)
		return
	}
	state := relay.ReadState(hook.DataDir())
	r.add(OK, "otel ignore", "%s; dropped since %s: %d export(s) with records from listed projects, %d with records naming no project (the last at %s)",
		listed, firstNonEmpty(state.CountingSince, state.StartedAt, "the receiver started"), state.Ignored, state.Unattributed,
		firstNonEmpty(state.LastIgnoredAt, "never"))
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// checkCodexRelay: Codex exports through the local relay. The table must carry the relay's
// key, the relay must answer and deliver an empty export, and no running Codex process may
// predate the table, since Codex reads it once at start.
func checkCodexRelay(ctx context.Context, r *Report, st install.State, table []byte) {
	dir := hook.DataDir()
	key := relay.ReadKey(dir)
	if key == "" || !bytes.Contains(table, []byte(strconv.Quote(relay.KeyHeader)+" = "+strconv.Quote(key))) {
		r.add(Fail, "codex otel", "the relay key in %s is not the one in %s, so the relay refuses every Codex export; re-run livy install",
			st.CodexConfig, filepath.Join(dir, relay.KeyFile))
		return
	}
	state := relay.ReadState(dir) // before the probe, which the relay counts too
	pctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	status, err := otlpProbe(pctx, "http://"+install.RelayListen, "/v1/logs", `{"resourceLogs":[]}`,
		map[string]string{relay.KeyHeader: key})
	switch {
	case err != nil:
		r.add(Fail, "codex otel", "the Codex relay on %s is not answering (%v): its %s service is not running, so every Codex export is lost; re-run livy install",
			install.RelayListen, err, st.CodexRelay.Kind)
		return
	case status < 200 || status >= 300:
		r.add(Fail, "codex otel", "the Codex relay could not deliver an empty export to the collector (HTTP %d): Codex telemetry is not arriving", status)
		return
	}
	r.add(OK, "codex otel", "exporting through the Codex relay on %s, which delivered an empty export just now", install.RelayListen)
	now := clock.Now()
	if t, err := time.Parse(time.RFC3339, state.LastFailedAt); err == nil && now.Sub(t) < 24*time.Hour {
		r.add(Warn, "codex otel relay", "%d Codex export(s) not delivered since %s, the last at %s: %s",
			state.Failed, state.CountingSince, state.LastFailedAt, state.LastFailure)
	}
	if t, err := time.Parse(time.RFC3339, state.LastRefusedAt); err == nil && now.Sub(t) < 24*time.Hour {
		r.add(Warn, "codex otel relay", "%d request(s) refused for a missing or wrong relay key since %s, the last at %s: a Codex process started before the key (restart it), Claude Code exporting with headers that are not Livy's (see the otel managed settings rows), or another local process",
			state.Refused, state.CountingSince, state.LastRefusedAt)
	}
	since, err := time.Parse(time.RFC3339, st.CodexRelaySince)
	if err != nil {
		r.add(Warn, "codex otel sessions", "the install did not record when Codex moved to the relay; re-run livy install")
		return
	}
	out, err := output(ctx, "ps", "-A", "-o", "pid=,etime=,comm=")
	if err != nil {
		r.add(Warn, "codex otel sessions", "could not list processes, so a Codex process still exporting with its old settings would not show here")
		return
	}
	if stale := StaleCodex(string(out), now, since); len(stale) > 0 {
		r.add(Fail, "codex otel sessions", "%d Codex process(es) started before Codex's export moved to the relay (pid %s): they export with the settings they read at start, which the collector refuses; quit and restart Codex (the app and any codex sessions)",
			len(stale), strings.Join(stale, ", "))
		return
	}
	r.add(OK, "codex otel sessions", "no running Codex process predates the relay")
}

// StaleCodex is the pids of `codex` processes in `ps -A -o pid=,etime=,comm=` output that
// started before since. Only the codex executable itself: its helpers export nothing.
func StaleCodex(ps string, now, since time.Time) []string {
	return staleProcesses(ps, "codex", now, since)
}

// staleProcesses is the pids of processes whose executable is named name in `ps -A -o
// pid=,etime=,comm=` output that started before since.
func staleProcesses(ps, name string, now, since time.Time) []string {
	var out []string
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || filepath.Base(strings.Join(f[2:], " ")) != name {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err != nil {
			continue
		}
		if age, ok := etime(f[1]); ok && now.Add(-age).Before(since) {
			out = append(out, f[0])
		}
	}
	return out
}

// etime reads ps's elapsed time, [[dd-]hh:]mm:ss.
func etime(s string) (time.Duration, bool) {
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(total)*time.Second, true
}

// oldestWaiting is when the oldest data still waiting to ship was captured, and whether
// any is waiting: a non-empty ledger, a spilled line, or a pending segment (empty folders
// left behind by shipping do not count). A file's time is its first line's ts, else its
// modification time.
func oldestWaiting(dir string) (oldest time.Time, any, unreadable bool) {
	consider := func(path string) {
		if t, ok := capturedAt(path); ok && (!any || t.Before(oldest)) {
			oldest, any = t, true
		}
	}
	consider(filepath.Join(dir, ledger.FileName))
	for _, sub := range []string{emit.SegmentsDir, ledger.SpillDir} {
		root := filepath.Join(dir, sub)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && errors.Is(err, fs.ErrNotExist) && p == root:
			case err != nil:
				unreadable = true
			case !d.IsDir() && strings.HasSuffix(p, ".jsonl"):
				consider(p)
			}
			return nil
		})
	}
	return oldest, any, unreadable
}

// capturedAt is when a non-empty file's data was captured: its first line's ts when that is
// earlier than its modification time, else that time; false for an empty or missing file.
func capturedAt(path string) (time.Time, bool) {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return time.Time{}, false
	}
	t := st.ModTime()
	if f, err := os.Open(path); err == nil {
		line, _ := bufio.NewReader(io.LimitReader(f, 64*1024)).ReadBytes('\n')
		_ = f.Close()
		var rec struct {
			TS string `json:"ts"`
		}
		if json.Unmarshal(line, &rec) == nil {
			if ts, err := time.Parse(time.RFC3339, rec.TS); err == nil && ts.Before(t) {
				t = ts
			}
		}
	}
	return t, true
}

// doctorArgs are Run's flags.
type doctorArgs struct {
	asJSON, raw, notify, errorsView bool
	keep                            int
	statusFile                      string
}

// parseArgs reads Run's flags; a non-zero code is the exit code for a bad one.
func parseArgs(args []string, stderr io.Writer) (doctorArgs, int) {
	o := doctorArgs{keep: 20}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "usage: livy doctor [--errors] [--errors-keep N] [--json] [--raw] [--status-file PATH] [--notify]\n"+
			"livy doctor: error: "+format+"\n", a...)
		return 2
	}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--json":
			o.asJSON = true
		case a == "--raw":
			o.raw = true
		case a == "--notify":
			o.notify = true
		case a == "--errors":
			o.errorsView = true
		case a == "--errors-keep" || strings.HasPrefix(a, "--errors-keep="):
			v := strings.TrimPrefix(a, "--errors-keep=")
			if a == "--errors-keep" {
				if i+1 >= len(args) {
					return o, usage("argument --errors-keep: expected one argument")
				}
				i++
				v = args[i]
			}
			n, err := strconv.Atoi(v)
			switch {
			case err != nil:
				return o, usage("argument --errors-keep: %q is not a whole number", v)
			case n < 1:
				return o, usage("argument --errors-keep: %q must be at least 1", v)
			}
			o.keep = n
		case a == "--status-file" && i+1 < len(args):
			i++
			o.statusFile = args[i]
		case strings.HasPrefix(a, "--status-file="):
			o.statusFile = strings.TrimPrefix(a, "--status-file=")
		default:
			return o, usage("unrecognized arguments: %s", a)
		}
	}
	return o, 0
}

// Run is `livy doctor [--json] [--status-file PATH]`: 0 healthy, 1 broken, 2 bad arguments.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, sinkFor func() (emit.Sink, error)) int {
	o, code := parseArgs(args, stderr)
	if code != 0 {
		return code
	}
	asJSON, raw, notifyOn, errorsView, keep, statusFile := o.asJSON, o.raw, o.notify, o.errorsView, o.keep, o.statusFile
	if errorsView {
		return ShowErrors(keep, stdout, stderr)
	}
	rep := Check(ctx, sinkFor)
	if ctx.Err() != nil {
		// Stopped: the rows would read "context canceled", which is not the machine's state.
		fmt.Fprintln(stderr, "doctor: stopped before the checks finished; nothing written")
		return 1
	}
	if statusFile != "" {
		if err := WriteStatus(statusFile, rep); err != nil {
			fmt.Fprintf(stderr, "doctor: could not write %s: %v\n", statusFile, err)
			hook.WriteError("doctor: could not write the status file: "+err.Error(), stderr)
		}
	}
	if notifyOn && !rep.OK {
		for _, row := range rep.Rows {
			if row.Status == Fail {
				notify(ctx, clip(row.Check+": "+redact(row.Detail), 120))
				break
			}
		}
	}
	if asJSON {
		out := rep
		if !raw {
			out.Rows = redactRows(rep.Rows)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintln(stdout, renderRows(rep))
	}
	if !rep.OK {
		return 1
	}
	return 0
}

func redactRows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		r.Detail = redact(r.Detail)
		out[i] = r
	}
	return out
}

// WriteStatus writes the daily check's result, redacted like --json since someone will
// paste it into an issue, through one fixed temp name.
func WriteStatus(path string, rep Report) error {
	out := struct {
		OK        bool           `json:"ok"`
		CheckedAt string         `json:"checked_at"`
		Failures  []statusDetail `json:"failures"`
		Warnings  []statusDetail `json:"warnings"`
		Checks    int            `json:"checks"`
	}{OK: rep.OK, CheckedAt: clock.Seconds(clock.Now()), Failures: []statusDetail{}, Warnings: []statusDetail{}, Checks: len(rep.Rows)}
	for _, r := range rep.Rows {
		d := statusDetail{Check: r.Check, Status: r.Status, Detail: redact(r.Detail)}
		switch r.Status {
		case Fail:
			out.Failures = append(out.Failures, d)
		case Warn:
			out.Warnings = append(out.Warnings, d)
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Status is the doctor.status event: check names and ok/warn/fail only, since the detail
// names paths, the handle and addresses.
func Status(rep Report) map[string]any {
	seen := map[string]string{}
	var order []string
	rank := map[string]int{OK: 0, Warn: 1, Fail: 2}
	for _, r := range rep.Rows {
		if cur, ok := seen[r.Check]; !ok || rank[r.Status] > rank[cur] {
			if !ok {
				order = append(order, r.Check)
			}
			seen[r.Check] = r.Status
		}
	}
	checks := make([]any, 0, len(order))
	for _, c := range order {
		checks = append(checks, map[string]any{"check": c, "status": seen[c]})
	}
	return map[string]any{"schema_version": int64(1), "ts": clock.Seconds(clock.Now()), "event": "doctor.status",
		"repo": "unknown", "doctor_ok": rep.OK, "doctor_checks": checks}
}

// renderRows is the text report: one `[ok  ] check  detail` row each, then the counts.
func renderRows(rep Report) string {
	mark := map[string]string{OK: "ok  ", Warn: "warn", Fail: "FAIL"}
	width := 0
	for _, r := range rep.Rows {
		width = max(width, len(r.Check))
	}
	var b strings.Builder
	n := map[string]int{}
	for _, r := range rep.Rows {
		fmt.Fprintf(&b, "[%s] %-*s  %s\n", mark[r.Status], width, r.Check, r.Detail)
		n[r.Status]++
	}
	fmt.Fprintf(&b, "\n%d ok, %d warning(s), %d failure(s)", n[OK], n[Warn], n[Fail])
	return b.String()
}
