// Package install is `livy install` and `livy uninstall`: one command on a machine wires
// the Claude Code user hooks, the per-repo git hook and the daily flush, and optionally
// OTEL export for Claude Code and Codex. It changes only entries carrying its own marker,
// leaves every other hook, permission and setting as it was, changes nothing on a dry run,
// and is idempotent. Uninstall removes exactly what install added and keeps captured data
// unless asked to purge. It never writes a credential.
package install

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xbew/livy/internal/amp"
	"github.com/0xbew/livy/internal/clock"
	"github.com/0xbew/livy/internal/config"
	"github.com/0xbew/livy/internal/denylist"
	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/extract"
	"github.com/0xbew/livy/internal/extract/claudecode"
	"github.com/0xbew/livy/internal/extract/cli"
	"github.com/0xbew/livy/internal/extract/codex"
	"github.com/0xbew/livy/internal/githook"
	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/ledger"
	"github.com/0xbew/livy/internal/otelignore"
	"github.com/0xbew/livy/internal/prompts"
	"github.com/0xbew/livy/internal/relay"
	"github.com/0xbew/livy/internal/sink/gcs"
	"github.com/0xbew/livy/internal/spotcheck"
)

// State is what install wrote, so uninstall removes exactly that. It holds paths and the
// values install set, never a secret.
type State struct {
	Binary          string            `json:"binary"`
	CommandLink     string            `json:"command_link,omitempty"`
	Settings        string            `json:"settings"`
	CreatedSettings bool              `json:"created_settings,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	HeadersHelper   string            `json:"otel_headers_helper,omitempty"`
	CodexConfig     string            `json:"codex_config,omitempty"`
	CodexRelay      Schedule          `json:"codex_relay,omitempty"`
	// When Codex's table last moved to the relay or its key: a Codex process started before
	// it still exports with what it read then.
	CodexRelaySince string `json:"codex_relay_since,omitempty"`
	// OtelDestination is where this install sends OTEL: sink (Livy's receiver) or collector.
	OtelDestination string   `json:"otel_destination,omitempty"`
	GitHooks        []string `json:"git_hooks,omitempty"`
	// HookRedirects are the repos whose tracked core.hooksPath install routed through Livy's wrappers.
	HookRedirects []hookRedirect `json:"hook_redirects,omitempty"`
	Schedule      Schedule       `json:"schedule"`
	// AmpPlugin is the Amp plugin file install wrote, and the sha256 of what it wrote, so an
	// edited file is kept.
	AmpPlugin       string `json:"amp_plugin,omitempty"`
	AmpPluginSHA256 string `json:"amp_plugin_sha256,omitempty"`
	// CodexHooks is the Codex hooks file install wrote Livy's entries to, and whether install
	// created it, so uninstall removes a file left holding nothing.
	CodexHooks        string `json:"codex_hooks,omitempty"`
	CreatedCodexHooks bool   `json:"created_codex_hooks,omitempty"`
}

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

type options struct {
	dry, noSchedule, noDiscover, purge bool
	orgs, repos                        listFlag
	handle, profile                    string
	otel, otelProtocol, otelHeaderEnv  string
	otelAuth                           string
	otelLogs, otelMetrics, otelTraces  string
	noCodexOtel                        bool
	noAmp                              bool // --no-amp: no Amp plugin (removes ours)
	noTrailers                         bool // --no-trailers: trailers.enabled=false, and no prepare-commit-msg hook
	otelTo                             string
	account, writerSA                  string
	sinkMode                           bool   // --otel-to sink: Livy's own receiver
	otelDefault                        bool   // the receiver is on because no flag said otherwise
	otelDefaultOff                     string // why the default receiver stays off, "" when it does not
	otelHeld                           string // why no OTEL export is set up at all, "" when it is not held
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parse(name string, args []string, stderr io.Writer) (options, error) {
	res := hook.Settings()
	configured, _ := res.Settings[otelDestinationKey].Value.(string)
	if configured == "" {
		// An install made with --otel <url> recorded its collector only in the install state.
		configured = loadState(where().statePath).OtelDestination
	}
	return parseWith(name, args, stderr, configured, otelHeld(res))
}

// otelHeld is why install sets up no OTEL export, "" when nothing holds it: a refused managed
// file may lock otel, as it may lock the sink.
func otelHeld(res config.Resolved) string {
	if !res.ManagedUnusable {
		return ""
	}
	return "held off, and any OTEL export an earlier install set up is removed: " + res.ManagedRefusal() +
		"; once it is fixed, re-run livy install"
}

// parseWith is parse with the otel.destination the config already holds, and why OTEL is held
// off ("" when it is not).
func parseWith(name string, args []string, stderr io.Writer, configured, held string) (options, error) {
	var o options
	fl := flag.NewFlagSet("livy "+name, flag.ContinueOnError)
	fl.SetOutput(stderr)
	fl.BoolVar(&o.dry, "dry-run", false, "print every change, make none")
	fl.Var(&o.repos, "repo", "a repo to put the git hook in (repeatable)")
	if name == "install" {
		installFlags(fl, &o)
	} else {
		fl.BoolVar(&o.purge, "purge", false, "also delete the captured data")
	}
	if err := fl.Parse(args); err != nil {
		return o, err
	}
	if fl.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fl.Arg(0))
	}
	if name == "install" {
		defaultReceiver(&o, configured)
	}
	err := otelFlags(name, &o)
	if err == nil && name == "install" && held != "" {
		holdOtel(&o, held)
	}
	return o, err
}

// holdOtel turns off every OTEL destination, the default receiver's and any flag's: the steps
// after it then remove the OTEL settings, Codex table and receiver an earlier install set up.
// A destination a flag names is still written to the user config, where it is held too.
func holdOtel(o *options, why string) {
	if o.otelDefault {
		o.otelTo = ""
	}
	o.otel, o.sinkMode, o.otelDefault, o.otelDefaultOff = "", false, false, ""
	o.otelAuth, o.otelHeaderEnv = "", ""
	o.otelHeld = why
}

// otelDestinationKey is the config key install records the OTEL destination under.
const otelDestinationKey = "otel.destination"

// otelNone is the --otel-to value, and the otel.destination, that keeps OTEL off.
const otelNone = "none"

// defaultReceiver turns Livy's own receiver on when no OTEL flag says otherwise, so a machine
// records OTEL from its first install. An opt-out recorded in the config holds on every later
// install; a configured collector is left as install has always left it. With no schedule or
// no service manager nothing would keep the receiver running, so it stays off and says why.
func defaultReceiver(o *options, configured string) {
	if o.otel != "" || o.otelTo != "" || (configured != "" && configured != "sink") {
		return
	}
	switch {
	case o.noSchedule:
		o.otelDefaultOff = "off: the receiver needs the schedule that --no-schedule leaves out"
	case relayHost() == "":
		o.otelDefaultOff = "off: no launch agent or systemd user service here to keep the receiver running"
	default:
		o.otelTo, o.otelDefault = "sink", true
	}
}

// parseInstall is parse for install with the otel.destination the config already holds, and
// why OTEL is held off ("" when it is not).
func parseInstall(args []string, configured, held string) (options, error) {
	return parseWith("install", args, io.Discard, configured, held)
}

// installFlags declares install's own flags.
func installFlags(fl *flag.FlagSet, o *options) {
	fl.Var(&o.orgs, "org", "an org in scope, written to the user config (repeatable)")
	fl.StringVar(&o.handle, "handle", "", "your handle, written to the user config (names your objects)")
	fl.StringVar(&o.profile, "config", "", "a JSON profile merged into the user config (orgs, sink, identity)")
	fl.BoolVar(&o.noCodexOtel, "no-codex-otel", false, "do not export Codex telemetry, even with --otel (removes ours)")
	fl.BoolVar(&o.noAmp, "no-amp", false, "do not record Amp: no plugin in ~/.config/amp/plugins (removes ours)")
	fl.BoolVar(&o.noSchedule, "no-schedule", false, "do not install the daily flush")
	fl.BoolVar(&o.noTrailers, "no-trailers", false,
		"do not stamp agent commits with attribution trailers (sets trailers.enabled false; removes Livy's prepare-commit-msg)")
	fl.BoolVar(&o.noDiscover, "no-discover", false, "do not look for repos in Claude Code's transcripts")
	fl.StringVar(&o.otel, "otel", "", "turn on OTEL export to this OTLP/HTTP endpoint (Claude Code and Codex)")
	fl.StringVar(&o.otelProtocol, "otel-protocol", "http/protobuf", "Claude Code's OTLP protocol")
	fl.StringVar(&o.otelLogs, "otel-logs", "otlp", "logs from Claude Code and Codex: otlp or none")
	fl.StringVar(&o.otelMetrics, "otel-metrics", "otlp", "metrics from Claude Code and Codex: otlp or none")
	fl.StringVar(&o.otelHeaderEnv, "otel-header-env", "",
		"name of an env var holding the OTLP Authorization header value (read at export time, never written)")
	fl.StringVar(&o.otelAuth, "otel-auth", "",
		"gcloud: send an ID token minted as the gcs sink's writer_sa, for a collector only the writer may invoke")
	fl.StringVar(&o.otelTraces, "otel-traces", "otlp", "traces from Claude Code and Codex: otlp or none")
	fl.StringVar(&o.account, "account", "", "the gcloud account the sink acts as, written to sink.account")
	fl.StringVar(&o.writerSA, "writer-sa", "", "the service account the sink impersonates, written to sink.writer_sa")
	fl.StringVar(&o.otelTo, "otel-to", "",
		"where OTEL goes: sink (Livy's own receiver on this machine, shipped through the configured sink; the default), "+
			"a collector URL, or none (off, on this and later installs)")
}

// otelDestination settles --otel-to into the endpoint: Livy's own receiver for sink, else
// the collector named, which --otel may repeat but not contradict.
func otelDestination(o *options) error {
	if o.otelTo == otelNone {
		if o.otel != "" || o.otelAuth != "" || o.otelHeaderEnv != "" {
			return errors.New("--otel-to none turns OTEL off; it takes no --otel, --otel-auth or --otel-header-env")
		}
		return nil
	}
	if o.otelTo != "" {
		if o.otelTo == "sink" {
			if o.otel != "" || o.otelAuth != "" || o.otelHeaderEnv != "" {
				return errors.New("--otel-to sink takes no --otel, --otel-auth or --otel-header-env: the receiver is on this machine")
			}
			// Livy's receiver decodes OTLP/JSON with the standard library.
			o.sinkMode, o.otel, o.otelProtocol = true, "http://"+RelayListen, "http/json"
		} else {
			if o.otel != "" && o.otel != o.otelTo {
				return errors.New("--otel-to and --otel name two destinations; pick one")
			}
			o.otel = o.otelTo
		}
	}
	return nil
}

// otelFlags checks the OTEL flags fit together, and settles --otel-to into the endpoint.
func otelFlags(name string, o *options) error {
	if err := otelDestination(o); err != nil {
		return err
	}
	if o.otel != "" && !o.sinkMode {
		u, err := url.Parse(o.otel)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("--otel %q is not an http(s) URL", o.otel)
		}
		if u.User != nil {
			return errors.New("--otel must not carry credentials; use --otel-header-env")
		}
	}
	for _, sig := range []struct{ flag, v string }{{"logs", o.otelLogs}, {"metrics", o.otelMetrics}, {"traces", o.otelTraces}} {
		if name == "install" && sig.v != "otlp" && sig.v != "none" {
			return fmt.Errorf("--otel-%s %q: want otlp or none", sig.flag, sig.v)
		}
	}
	if o.otelHeaderEnv != "" && !envName.MatchString(o.otelHeaderEnv) {
		return fmt.Errorf("--otel-header-env %q is not a variable name", o.otelHeaderEnv)
	}
	if o.otelAuth != "" && o.otelAuth != "gcloud" {
		return fmt.Errorf("--otel-auth %q: want gcloud", o.otelAuth)
	}
	if o.otelAuth != "" && o.otelHeaderEnv != "" {
		return errors.New("--otel-auth and --otel-header-env each set the Authorization header; pick one")
	}
	return nil
}

// paths install works with.
type paths struct {
	home, userDir, bin, statePath, settings, codex string
}

func where() paths {
	home := hook.Home()
	userDir := filepath.Dir(hook.UserConfigPath())
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return paths{home: home, userDir: userDir, bin: filepath.Join(home, ".livy", "bin", "livy"),
		statePath: filepath.Join(userDir, "install.json"), settings: filepath.Join(claude, "settings.json"),
		codex: filepath.Join(codex, "config.toml")}
}

// Install is `livy install [flags]`: 0 when every part is in place, 1 when a part failed
// (said on stdout), 2 on bad arguments.
func Install(args []string, stdout, stderr io.Writer) int {
	for i, a := range args {
		if a == "--uninstall" {
			return Uninstall(append(append([]string(nil), args[:i]...), args[i+1:]...), stdout, stderr)
		}
	}
	o, err := parse("install", translatePyArgs(args), stderr)
	if err != nil {
		return 2
	}
	r := &installRun{o: o, p: where(), stdout: stdout, stderr: stderr}
	if !r.loadRecord() || !r.binary() {
		return 1
	}
	r.userConfig()
	receiver := r.receiver()
	r.claudeCode()
	r.codex(receiver)
	r.codexHooks()
	r.amp()
	r.gitHooks()
	r.schedule()
	r.say("denylist: %s", denylist.InstallStep(r.o.dry, stderr)) // 7. after the config that names its source
	if !r.o.dry {
		if err := saveState(r.p.statePath, r.st); err != nil {
			r.fail("state: could not write %s (%v); uninstall will fall back to markers", r.p.statePath, err)
		}
		r.say("next: run `%s doctor` to prove capture is alive", r.p.bin)
	}
	if r.failed {
		return 1
	}
	return 0
}

// installRun is one Install: what an earlier install owns (prev), what this one will own
// (st), and whether any step failed.
type installRun struct {
	o              options
	p              paths
	prev, st       State
	stdout, stderr io.Writer
	failed         bool
}

func (r *installRun) say(format string, a ...any) { fmt.Fprintf(r.stdout, format+"\n", a...) }

func (r *installRun) fail(format string, a ...any) {
	r.failed = true
	r.say(format, a...)
}

// loadRecord reads the earlier install's record; false when an unreadable one cannot be
// set aside.
func (r *installRun) loadRecord() bool {
	prev, stateErr := readState(r.p.statePath)
	r.prev = prev
	r.st = prev // what an earlier install owns stays owned unless this run removes it
	r.st.Binary, r.st.Settings = r.p.bin, r.p.settings
	if stateErr == nil {
		return true
	}
	r.say("install record: %s is not readable (%v), so what an earlier install owns is unknown", r.p.statePath, stateErr)
	if r.o.dry {
		return true
	}
	// Kept, not overwritten: the record is what uninstall reads to know which keys and helpers are ours.
	if err := os.Rename(r.p.statePath, fmt.Sprintf("%s.corrupt-%d", r.p.statePath, time.Now().Unix())); err != nil {
		r.fail("install record: could not set it aside: %v", err)
		return false
	}
	r.say("install record: kept beside it as %s.corrupt-<time>", r.p.statePath)
	return true
}

// binary is step 1: the binary, at a fixed path the hooks name, so an upgrade replaces code
// only. Its folder is created private first: MkdirAll would otherwise make ~/.livy 0755.
// False when the binary could not be placed.
func (r *installRun) binary() bool {
	own, shared := ownFolders(r.p, hook.DataDir())
	for _, n := range MakePrivate(own, shared, r.o.dry) {
		if n.Failed {
			r.fail("data: %s", n.Text)
			continue
		}
		r.say("data: %s", n.Text)
	}
	msg, err := placeBinary(r.p.bin, r.o.dry)
	if err != nil {
		r.fail("binary: %v", err)
		return false
	}
	r.say("binary: %s", msg)
	link, linkMsg, linkBad := installLink(r.p.home, r.p.bin, r.o.dry)
	r.failed = r.failed || linkBad
	r.say("%s", linkMsg)
	if link != "" {
		if r.prev.CommandLink != "" && r.prev.CommandLink != link && !r.o.dry {
			_ = removeLink(r.prev.CommandLink, r.p.bin, false) // an earlier link in another folder
		}
		r.st.CommandLink = link
	}
	return true
}

// userConfig is step 2: only the keys given here; everything else in it is kept.
func (r *installRun) userConfig() {
	o := r.o
	if o.profile != "" || len(o.orgs) > 0 || o.handle != "" || (o.otelTo != "" && !o.otelDefault) || o.account != "" || o.writerSA != "" || o.noTrailers {
		msg, err := writeConfig(hook.UserConfigPath(), o, o.dry)
		if err != nil {
			r.fail("config: %v", err)
		} else {
			r.say("config: %s", msg)
		}
	}
	if msg, err := ensureSink(r.p.home, o.dry, o.profile); err != nil {
		r.fail("sink: %v; data stays in the data folder", err)
	} else if msg != "" {
		r.say("sink: %s", msg)
	}
	if !o.dry {
		if _, err := emit.FromSettings(hook.Settings()); err != nil && !errors.Is(err, emit.ErrNoSink) {
			r.fail("sink: %v; every upload is refused until it is fixed", err)
		}
	}
}

// receiver is step 2b: Livy's OTEL receiver, for --otel-to sink, loaded and answering
// before any client is pointed at it, since an export sent while nothing listens is lost.
func (r *installRun) receiver() Schedule {
	if r.o.otelHeld != "" {
		r.say("otel: %s", r.o.otelHeld)
	}
	if r.o.otelDefaultOff != "" {
		r.say("otel: %s; `livy install --otel-to sink` turns it on, --otel-to none silences this", r.o.otelDefaultOff)
	}
	if !r.o.sinkMode {
		return Schedule{}
	}
	receiver, msg, bad := loadReceiver(r.p, r.o, r.prev)
	r.say("otel: %s", msg)
	if bad {
		// A receiver asked for fails the install. The default one is said here and in doctor's
		// otel receiver row instead: an install nobody asked OTEL of still installs the rest.
		r.failed = r.failed || !r.o.otelDefault
		r.o.otel, r.o.sinkMode = "", false // not configured: the steps below remove ours
	}
	return receiver
}

// claudeCode is step 3: hooks, and OTEL when asked, in the user settings file.
func (r *installRun) claudeCode() {
	settings, created, err := readSettings(r.p.settings)
	rawSettings, _ := os.ReadFile(r.p.settings)
	if err != nil {
		r.fail("claude code: %s is not usable (%v); left alone, no hooks installed", r.p.settings, err)
		return
	}
	// Ownership is recorded only once the file holding it is written: `next` is what
	// this run would own, and st takes it after the write succeeds.
	next := r.st
	before := encode(settings)
	if refused := mergeHooks(settings, desiredHooks(r.p.bin)); len(refused) > 0 {
		r.fail("claude code: hooks for %s hold something that is not a list; left alone", strings.Join(refused, ", "))
	}
	switch {
	case r.o.otel != "":
		r.claudeOtel(settings, &next)
	case r.prev.Env != nil || r.prev.HeadersHelper != "":
		unsetOwned(settings, "env", r.prev.Env) // OTEL was on and is not asked for now
		dropHelper(settings, r.prev.HeadersHelper)
		next.Env, next.HeadersHelper = nil, ""
	}
	next.CreatedSettings = created || r.prev.CreatedSettings
	switch {
	case string(before) == string(encode(settings)) && !created:
		r.say("claude code: hooks already in place in %s", r.p.settings)
		r.st = next
	case r.o.dry:
		r.say("claude code: would update %s", r.p.settings)
	default:
		if err := writeFileAtomic(r.p.settings, encodeLike(rawSettings, settings), 0o600); err != nil {
			r.fail("claude code: could not write %s: %v", r.p.settings, err)
		} else {
			r.say("claude code: hooks written to %s", r.p.settings)
			r.st = next
		}
	}
}

// claudeOtel sets the OTEL env keys and headers helper install owns, and drops the ones it
// owned before and no longer wants.
func (r *installRun) claudeOtel(settings *object, next *State) {
	want := otelEnv(r.o, handleFor(r.o))
	stale := map[string]string{}
	for k, v := range r.prev.Env {
		if _, still := want[k]; !still {
			stale[k] = v
		}
	}
	unsetOwned(settings, "env", stale)
	env, skipped := setOwned(settings, "env", want, r.prev.Env)
	next.Env = env
	for _, s := range skipped {
		r.say("otel: %s left alone", s)
	}
	helper := helperFor(r.p.bin, r.o)
	cur, has := settings.get("otelHeadersHelper")
	s, isStr := cur.(string)
	mine := has && isStr && r.prev.HeadersHelper != "" && s == r.prev.HeadersHelper
	switch {
	case helper == "" && mine:
		settings.del("otelHeadersHelper")
		next.HeadersHelper = ""
	case helper == "":
	case !has || mine:
		settings.set("otelHeadersHelper", helper)
		next.HeadersHelper = helper
	default:
		r.say("otel: otelHeadersHelper holds a value install did not set; left alone")
	}
}

// codex is step 4: Codex OTEL, only when Codex is on this machine and OTEL is asked for.
func (r *installRun) codex(receiver Schedule) {
	o, prev := r.o, r.prev
	r.st.CodexRelay = receiver // the receiver in sink mode; set again below when this run keeps the Codex relay
	r.st.OtelDestination = ""
	switch {
	case o.sinkMode:
		r.st.OtelDestination = "sink"
	case o.otel != "":
		r.st.OtelDestination = "collector"
	}
	codexAsked := o
	if o.noCodexOtel {
		codexAsked.otel, codexAsked.sinkMode = "", false
	}
	msg, bad := codexOtel(r.p, codexAsked, prev, &r.st)
	r.keepOrRemoveRelay()
	if msg == "" {
		return
	}
	if bad {
		r.failed = true
	}
	if strings.HasPrefix(msg, "codex: wrote") || strings.HasPrefix(msg, "codex: would") || strings.HasPrefix(msg, "codex: already") ||
		strings.HasPrefix(msg, "codex: exporting") {
		r.st.CodexConfig = r.p.codex
	}
	r.say("%s", msg)
}

// keepOrRemoveRelay removes an earlier Codex relay this run no longer uses, unless Codex's
// config still sends to it.
func (r *installRun) keepOrRemoveRelay() {
	prev := r.prev
	if r.st.CodexRelay.Kind == "" && prev.CodexRelay.Kind != "" && codexNamesRelay(r.p.codex) && !r.o.dry {
		// This run neither rewrote nor removed a table that still sends to the relay (an
		// unreadable file, a failed key write): the relay keeps running and stays recorded.
		r.st.CodexRelay = prev.CodexRelay
	}
	if r.st.CodexRelay.Kind != "" || prev.CodexRelay.Kind == "" {
		return
	}
	if r.o.dry {
		r.st.CodexRelay = prev.CodexRelay
	} else {
		r.st.CodexRelaySince = ""
	}
	for _, m := range removeRelay(prev.CodexRelay, r.o.dry) {
		r.say("codex: %s", m)
	}
}

// gitHooks is step 5: the git hook in each in-scope repo named or used with Claude Code.
func (r *installRun) gitHooks() {
	r.st.HookRedirects = nil // rebuilt below from this run and the earlier record
	cmd := hookCmd{bin: r.p.bin, trailers: !r.o.noTrailers && githook.TrailersEnabled()}
	for _, repo := range KnownRepos(r.prev) {
		if _, err := os.Stat(repo); errors.Is(err, fs.ErrNotExist) {
			r.say("git: %s: gone since an earlier install hooked it; nothing to put back", repo)
			if !r.o.dry {
				r.st.GitHooks = slices.DeleteFunc(slices.Clone(r.st.GitHooks), func(h string) bool { return RepoOfHooks(filepath.Dir(h)) == repo })
			}
		}
	}
	for _, repo := range installRepos(r.o, r.p, r.prev) {
		r.gitHook(repo, cmd)
	}
	for _, h := range r.prev.HookRedirects {
		if _, err := os.Stat(h.GitDir); err == nil && !r.hasRedirect(h.GitDir) {
			r.st.HookRedirects = append(r.st.HookRedirects, h)
		}
	}
	for _, h := range r.prev.GitHooks {
		if _, err := os.Stat(h); err == nil {
			r.st.GitHooks = appendNew(r.st.GitHooks, h)
		}
	}
}

func (r *installRun) hasRedirect(gitDir string) bool {
	for _, n := range r.st.HookRedirects {
		if n.GitDir == gitDir {
			return true
		}
	}
	return false
}

func (r *installRun) gitHook(repo string, cmd hookCmd) {
	var before *hookRedirect
	for i := range r.prev.HookRedirects {
		if r.prev.HookRedirects[i].GitDir == commonDir(repo) {
			before = &r.prev.HookRedirects[i]
		}
	}
	if before == nil && routedThroughLivy(repo, r.p.home) {
		r.say("git: %s: shares a routed core.hooksPath with another checkout of the repo; left to that record", repo)
		return
	}
	if _, tracked, err := hookPath(repo); before != nil || (err == nil && tracked) {
		rec, msg, bad := installRedirect(repo, cmd, r.p.home, before, r.o.dry)
		r.failed = r.failed || bad
		r.say("git: %s", msg)
		if rec != nil && !r.hasRedirect(rec.GitDir) {
			r.st.HookRedirects = append(r.st.HookRedirects, *rec)
		}
		return
	}
	r.recordHook(installGitHook(repo, cmd.bin, r.o.dry))
	r.recordHook(installTrailerHook(repo, cmd.bin, cmd.trailers, r.o.dry))
	for _, h := range installCommitHooks(repo, cmd.bin, r.o.dry) {
		r.recordHook(h.path, h.msg, h.failed)
	}
}

// recordHook says what one hook step did and owns the file it wrote.
func (r *installRun) recordHook(path, msg string, bad bool) {
	r.failed = r.failed || bad
	if msg != "" {
		r.say("git: %s", msg)
	}
	if path != "" {
		r.st.GitHooks = appendNew(r.st.GitHooks, path)
	}
}

// schedule is step 6: the daily flush.
func (r *installRun) schedule() {
	if r.o.noSchedule {
		if r.st.Schedule.Kind == "" {
			r.st.Schedule = Schedule{Kind: "none"}
		}
		r.say("schedule: skipped (--no-schedule); an earlier daily flush is kept")
		return
	}
	sched, msg, bad := installSchedule(r.p.home, r.p.bin, r.o.dry)
	if bad {
		r.failed = true
	}
	r.st.Schedule = sched
	r.say("schedule: %s", msg)
}

// dataNames are the files and folders Livy keeps in its data folder, from the list each
// writing package keeps beside its own names. Purge deletes these and nothing else, so a
// data folder pointed at a shared place (LIVY_DIR=~/Documents) loses only Livy's files.
// Uninstall does not purge the Python tool's folder, which uses the same names.
func dataNames() []string {
	return slices.Concat(ledger.DataNames(), emit.DataNames(), prompts.DataNames(), relay.DataNames(),
		spotcheck.DataNames(), extract.DataNames(), cli.DataNames(), claudecode.DataNames(),
		codex.DataNames(), denylist.DataNames(), amp.DataNames(), []string{dogfoodParkDir})
}

const (
	// dogfoodParkDir holds a dogfood machine's unshipped segments after migrate, in the data folder.
	dogfoodParkDir = "dogfood-unshipped"
	// doctorStatusName is the daily job's doctor verdict, in ~/.livy.
	doctorStatusName = "doctor-status.json"
	// migrateDir holds migrate's record, in the user folder.
	migrateDir = "migrate"
)

// userNames are the files and folders Livy keeps in the user folder (~/.livy) besides the
// data. recordNames are the ones a second uninstall needs when this one did not finish:
// the install record and the binary its remaining hooks call.
var (
	userNames   = []string{"config.json", "dogfood.json", doctorStatusName, migrateDir}
	recordNames = []string{"install.json", "bin"}
)

// purge deletes Livy's files from the data folder and from the user folder, which is
// Livy's own and may hold data from before LIVY_DIR moved it. It never sweeps pythonDir, the
// Python tool's folder, whichever role it plays. left is every entry or folder it kept.
func purge(dataDir, userDir, pythonDir string, keepRecord bool) (left []string, err error) {
	user, kept := slices.Concat(dataNames(), userNames), []string(nil)
	if keepRecord {
		kept = recordNames
	} else {
		user = append(user, recordNames...)
	}
	targets := []purgeTarget{{userDir, user, kept}}
	if filepath.Clean(dataDir) != filepath.Clean(userDir) {
		targets = append([]purgeTarget{{dataDir, dataNames(), nil}}, targets...)
	}
	for _, t := range targets {
		if same, _ := sameFolder(t.dir, pythonDir); same && pythonDir != "" {
			// The Python tool writes the same names here; its unshipped ledger is not Livy's to delete.
			left = append(left, t.dir+" (the Python tool's folder, which uses the same file names)")
			continue
		}
		more, err := purgeDir(t.dir, t.names, t.kept)
		left = append(left, more...)
		if err != nil {
			return left, err
		}
	}
	return left, nil
}

// purgeTarget is one folder purge sweeps: the names to delete, and those to keep unreported.
type purgeTarget struct {
	dir         string
	names, kept []string
}

// purgeDir deletes names from dir and reports what else it holds, except kept, which stays
// unreported.
func purgeDir(dir string, names, kept []string) (left []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ours := map[string]bool{}
	for _, n := range names {
		ours[n] = true
	}
	held := false
	for _, e := range entries {
		if slices.Contains(kept, e.Name()) {
			held = true
			continue
		}
		if !ownedName(e.Name(), ours) {
			left = append(left, filepath.Join(dir, e.Name()))
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return left, err
		}
	}
	if len(left) == 0 && !held {
		_ = os.Remove(dir)
	}
	return left, nil
}

// ownedName is whether n is one of ours, or the temp file or lock of one: name.tmp,
// name.livy-tmp, name.lock, name.<pid>, or os.CreateTemp's .name.<random>.tmp.
func ownedName(n string, ours map[string]bool) bool {
	if ours[n] {
		return true
	}
	if i := strings.LastIndex(n, "."); i > 0 && ours[n[:i]] && isDigits(n[i+1:]) {
		return true
	}
	for _, suffix := range []string{".tmp", ".livy-tmp", ".lock"} {
		base, ok := strings.CutSuffix(n, suffix)
		if !ok {
			continue
		}
		if ours[base] {
			return true
		}
		if hidden, ok := strings.CutPrefix(base, "."); ok {
			if i := strings.LastIndex(hidden, "."); i > 0 && ours[hidden[:i]] {
				return true
			}
		}
	}
	return false
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// OtelHeaders is `livy otel-headers NAME | --gcloud`, Claude Code's otelHeadersHelper,
// which Claude Code runs at start and about every 29 minutes. NAME prints the Authorization
// header from that variable at export time, so no secret is written to a settings file;
// nothing is printed when the variable is unset. --gcloud prints a bearer ID token minted
// as the gcs sink's writer (see gcloudHeaders).
func OtelHeaders(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--gcloud" {
		return gcloudHeaders(stdout, stderr)
	}
	if len(args) == 1 && args[0] == "--relay-key" {
		// Livy's receiver: the key it requires, read at export time, never in a settings file.
		key := relay.ReadKey(hook.DataDir())
		if key == "" {
			fmt.Fprintln(stderr, "otel-headers: no relay key in the data folder (re-run livy install --otel-to sink)")
			hook.WriteError("otel-headers: no relay key in the data folder; Livy's OTEL receiver will refuse every export", stderr)
			return 0
		}
		h := map[string]string{relay.KeyHeader: key}
		// Claude Code runs this in the session's folder: the receiver judges otel.ignore by it.
		if cwd, err := os.Getwd(); err == nil {
			h[otelignore.CwdHeader] = otelignore.EncodeCwd(cwd)
		}
		b, _ := json.Marshal(h)
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	if len(args) != 1 || !envName.MatchString(args[0]) {
		return 2
	}
	v := os.Getenv(args[0])
	if v == "" {
		fmt.Fprintln(stdout, "{}")
		return 0
	}
	b, _ := json.Marshal(map[string]string{"Authorization": v})
	fmt.Fprintln(stdout, string(b))
	return 0
}

// gcloudHeaders prints {"Authorization": "Bearer <ID token>"}: a token minted as the gcs
// sink's writer, with the collector's URL as its audience, good for an hour, so Claude Code's
// refresh keeps a valid one. The audience is the endpoint install recorded, else the
// exporter's: a shell that exports another tool's endpoint must not mint for that one.
// On any failure it prints nothing (a bogus header would hide the cause behind a 401), says
// why on stderr, writes a ledger.error, and exits 0.
func gcloudHeaders(stdout, stderr io.Writer) int {
	fail := func(why string) int {
		fmt.Fprintln(stderr, "otel-headers: "+why)
		hook.WriteError("otel-headers: "+why+"; telemetry export will be refused", stderr)
		return 0
	}
	st, _ := Recorded()
	audience := st.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if audience == "" {
		audience = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if audience == "" {
		return fail("no OTEL endpoint installed (livy install --otel)")
	}
	c, err := emit.GCSConfig(hook.Settings())
	if err != nil {
		return fail(err.Error())
	}
	tok, err := gcs.IDToken(context.Background(), c, audience)
	if err != nil {
		return fail(err.Error())
	}
	b, _ := json.Marshal(map[string]string{"Authorization": "Bearer " + tok})
	fmt.Fprintln(stdout, string(b))
	return 0
}

// codexSignals are the signals Codex exports, from the same opt-outs as Claude Code's.
func codexSignals(o options) signals {
	return signals{exporterOf(o.otelLogs) == "otlp", exporterOf(o.otelMetrics) == "otlp", exporterOf(o.otelTraces) == "otlp"}
}

// exporterOf is a signal's exporter: every signal is on unless opted out with none.
func exporterOf(v string) string {
	if v == "none" {
		return "none"
	}
	return "otlp"
}

func otelEnv(o options, handle string) map[string]string {
	env := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_LOGS_EXPORTER":           exporterOf(o.otelLogs),
		"OTEL_METRICS_EXPORTER":        exporterOf(o.otelMetrics),
		"OTEL_TRACES_EXPORTER":         exporterOf(o.otelTraces),
		"OTEL_EXPORTER_OTLP_PROTOCOL":  o.otelProtocol,
		"OTEL_EXPORTER_OTLP_ENDPOINT":  strings.TrimSuffix(o.otel, "/"),
	}
	if handle != "" {
		env["OTEL_RESOURCE_ATTRIBUTES"] = "engineer.handle=" + handle
	}
	if exporterOf(o.otelTraces) == "otlp" {
		// Claude Code exports spans only with its enhanced-telemetry beta on as well.
		env["CLAUDE_CODE_ENHANCED_TELEMETRY_BETA"] = "1"
	}
	return env
}

// handleFor is the handle the user config will hold after this install.
func handleFor(o options) string {
	if o.handle != "" {
		return o.handle
	}
	s, _ := hook.Settings().Settings["identity.handle"].Value.(string)
	return s
}

func dropHelper(settings *object, helper string) {
	if helper == "" {
		return
	}
	if cur, _ := settings.m["otelHeadersHelper"].(string); cur == helper {
		settings.del("otelHeadersHelper")
	}
}

// codexNamesRelay says Codex's config still holds our table sending to the relay.
func codexNamesRelay(path string) bool {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true // unreadable: assume it still does, and keep the relay
	}
	text := string(raw)
	return strings.Contains(text, codexMarker) && strings.Contains(text, "http://"+RelayListen+"/v1/logs")
}

// helperFor is Claude Code's otelHeadersHelper for these options, "" for none. The Codex
// relay runs the same command for its headers.
func helperFor(bin string, o options) string {
	switch {
	case o.otelHeaderEnv != "":
		return shellQuote(bin) + " otel-headers " + o.otelHeaderEnv
	case o.otelAuth == "gcloud":
		return shellQuote(bin) + " otel-headers --gcloud"
	case o.sinkMode:
		return shellQuote(bin) + " otel-headers --relay-key"
	}
	return ""
}

func codexOtel(p paths, o options, prev State, st *State) (string, bool) {
	if _, err := os.Stat(filepath.Dir(p.codex)); err != nil {
		if o.otel != "" {
			return "codex: not on this machine (" + filepath.Dir(p.codex) + "), skipped", false
		}
		return "", false
	}
	raw, err := os.ReadFile(p.codex)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("codex: %s unreadable (%v), left alone", p.codex, err), true
	}
	text := string(raw)
	ours := strings.Contains(text, codexMarker)
	if o.otel == "" {
		if !ours {
			return "", false
		}
		return removeCodexTable(p, o, text, "codex: would remove the [otel] table (OTEL not asked for)",
			"codex: removed the [otel] table (OTEL not asked for)")
	}
	if codexHasForeignOtel(text) {
		return "codex: " + p.codex + " already has an [otel] table that is not ours; left alone", false
	}
	if o.sinkMode {
		return codexToReceiver(p, o, prev, st, text)
	}
	if o.otelAuth == "gcloud" {
		if kind := relayHost(); kind != "" && !o.noSchedule {
			return codexRelay(p, o, prev, st, kind, text)
		}
		// Codex reads its headers once, at start, and an ID token lasts an hour: a table
		// carrying one would export into refusals for every Codex process after its first hour.
		// A table of ours from an earlier install goes, with any header it holds.
		why := "codex: not configured: Codex reads its export headers once at start and an ID token lasts an hour, " +
			"and the relay that refreshes it needs a launch agent or systemd user service (not with --no-schedule)"
		if !ours {
			return why + "; " + p.codex + " left as it was", false
		}
		return removeCodexTable(p, o, text, why+"; would remove the [otel] table an earlier install wrote",
			why+"; removed the [otel] table an earlier install wrote")
	}
	return codexDirect(p, o, text)
}

// removeCodexTable takes our [otel] table out of Codex's config, or says it would.
func removeCodexTable(p paths, o options, text, wouldSay, didSay string) (string, bool) {
	if o.dry {
		return wouldSay, false
	}
	if err := writeFileAtomic(p.codex, []byte(withoutCodexBlock(text)), 0o600); err != nil {
		return fmt.Sprintf("codex: could not write %s: %v", p.codex, err), true
	}
	return didSay, false
}

// codexAuth is the Authorization value Codex's table carries, and what to say about it.
// Codex reads export headers only literally, so the header value is written into its
// config (mode 0600), and said; Claude Code reads it from the variable at export time.
func codexAuth(p paths, o options) (auth, note string) {
	if o.otelHeaderEnv == "" {
		return "", ""
	}
	auth = os.Getenv(o.otelHeaderEnv)
	switch {
	case !tomlSafe(auth):
		return "", " (" + o.otelHeaderEnv + " holds control characters or invalid UTF-8; not written, so Codex gets no Authorization header)"
	case auth == "":
		return "", " (" + o.otelHeaderEnv + " is not set now, so Codex gets no Authorization header)"
	}
	return auth, " (Codex cannot read headers from the environment: the Authorization header from " +
		o.otelHeaderEnv + " is written into " + p.codex + ", mode 0600)"
}

// codexDirect points Codex's table straight at the collector.
func codexDirect(p paths, o options, text string) (string, bool) {
	auth, note := codexAuth(p, o)
	want := withCodexBlock(text, codexBlock(o.otel, "Authorization", auth, handleFor(o), codexSignals(o)))
	if o.dry && want != text {
		return "codex: would write the [otel] table in " + p.codex + note, false
	}
	if auth != "" && !o.dry {
		// A file holding a secret is 0600 before the secret is in it, and stays so.
		real := p.codex
		if r, err := filepath.EvalSymlinks(p.codex); err == nil {
			real = r
		}
		if err := os.Chmod(real, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("codex: could not make %s 0600 (%v); the header was not written", p.codex, err), true
		}
	}
	if want == text {
		return "codex: already exporting to " + o.otel, false
	}
	if err := writeFileAtomic(p.codex, []byte(want), 0o600); err != nil {
		return fmt.Sprintf("codex: could not write %s: %v", p.codex, err), true
	}
	return "codex: wrote the [otel] table in " + p.codex + note, false
}

// codexRelay loads the relay's service, then points Codex's table at it with the relay key,
// in that order: a table naming a relay that is not running exports into nothing. When the
// service does not load, our table is removed and the install is incomplete.
func codexRelay(p paths, o options, prev State, st *State, kind, text string) (string, bool) {
	if o.dry {
		st.CodexRelay = prev.CodexRelay
		return "codex: would load the Codex relay (" + kind + ") on " + RelayListen + " and point the [otel] table in " +
			p.codex + " at it", false
	}
	key, err := relayKey(hook.DataDir(), text)
	if err != nil {
		return fmt.Sprintf("codex: not configured: could not write the relay key: %v", err), true
	}
	loadedAt := clock.Now()
	sched, err := loadRelay(kind, p.home, p.bin, relayArgs(o.otel, helperFor(p.bin, o)))
	if err == nil && !relayListening(loadedAt) {
		removeRelay(sched, false)
		err = errors.New("it loaded but is not listening on " + RelayListen + " (the port taken?)")
	}
	if err != nil {
		msg := "codex: not configured: the Codex relay did not load (" + err.Error() + ")"
		if strings.Contains(text, codexMarker) {
			if werr := writeFileAtomic(p.codex, []byte(withoutCodexBlock(text)), 0o600); werr != nil {
				msg += fmt.Sprintf("; could not remove the old [otel] table from %s: %v", p.codex, werr)
			} else {
				msg += "; removed the old [otel] table"
			}
		}
		return msg, true
	}
	st.CodexRelay = sched
	want := withCodexBlock(text, codexBlock("http://"+RelayListen, relay.KeyHeader, key, handleFor(o), codexSignals(o)))
	if want == text {
		if prev.CodexRelaySince == "" {
			st.CodexRelaySince = clock.Now().UTC().Format(time.RFC3339)
		}
		return "codex: already exporting through the Codex relay (" + kind + ") to " + o.otel, false
	}
	// The table carries the relay key: never group or world readable.
	if err := writeFileAtomic(p.codex, []byte(want), 0o600); err != nil {
		return fmt.Sprintf("codex: could not write %s: %v", p.codex, err), true
	}
	st.CodexRelaySince = clock.Now().UTC().Format(time.RFC3339)
	return "codex: exporting through the Codex relay (" + kind + ", " + RelayListen + ") to " + o.otel +
		"; restart Codex (the app and any codex sessions) so it reads the new table", false
}

// tomlSafe says a header value can be written as a TOML basic string through
// strconv.Quote: valid UTF-8 with no control characters, whose Go escapes TOML rejects.
func tomlSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// installRepos are the repos an install puts Livy's git hooks in: those repos() finds, the
// folder install runs in, and every repo an earlier install hooked, so a re-install brings each
// one up to the hooks this version installs whether or not a transcript still names it.
func installRepos(o options, p paths, prev State) []string {
	extra := KnownRepos(prev)
	if wd, err := os.Getwd(); err == nil {
		extra = append([]string{wd}, extra...)
	}
	return repos(o, p, extra...)
}

// KnownRepos are the repos an install's state says it put hooks in: each hook file's repo and
// each redirected repo, in that order.
func KnownRepos(st State) []string {
	var out []string
	for _, h := range st.GitHooks {
		out = appendNew(out, RepoOfHooks(filepath.Dir(h)))
	}
	for _, r := range st.HookRedirects {
		out = appendNew(out, RepoOfHooks(r.GitDir))
	}
	return out
}

// RepoOfHooks is the repo a hooks folder (or a redirected repo's .git folder) belongs to: the
// work tree above <repo>/.git/hooks or <repo>/.git, else the folder itself (one inside the work
// tree, which git resolves).
func RepoOfHooks(dir string) string {
	switch {
	case filepath.Base(dir) == "hooks" && filepath.Base(filepath.Dir(dir)) == ".git":
		return filepath.Dir(filepath.Dir(dir))
	case filepath.Base(dir) == ".git":
		return filepath.Dir(dir)
	}
	return dir
}

// IsRepo is whether path is the top of a git work tree.
func IsRepo(path string) bool {
	top, err := gitOut(path, "rev-parse", "--show-toplevel")
	return err == nil && top == filepath.Clean(path)
}

// Revisited is which of dirs (hooks folders, or redirected repos' .git folders) a plain
// `livy install` brings up to date: their repos, in scope. The rest are folders no in-scope
// repo owns (a shared core.hooksPath, a repo out of scope), which install cannot fix by repo.
func Revisited(dirs []string) (fixable, not []string) {
	for _, d := range dirs {
		if repo := RepoOfHooks(d); len(repos(options{noDiscover: true}, paths{}, repo)) == 1 {
			fixable = append(fixable, repo)
		} else {
			not = append(not, d)
		}
	}
	return fixable, not
}

// repos are the named repos and the extra candidates plus, unless --no-discover, the in-scope
// repos Claude Code ran in. Only repos whose origin is under a configured org get the hook.
func repos(o options, p paths, extra ...string) []string {
	cands := append(append([]string(nil), o.repos...), extra...)
	if !o.noDiscover {
		claude := filepath.Dir(p.settings)
		cands = append(cands, transcriptRepos(claude)...)
	}
	orgs := map[string]bool{}
	for _, org := range append(hook.Orgs(io.Discard), lower(o.orgs)...) {
		orgs[org] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range cands {
		top, err := gitOut(r, "rev-parse", "--show-toplevel")
		if err != nil || top == "" || seen[top] {
			continue
		}
		seen[top] = true
		origin, _ := gitOut(top, "remote", "get-url", "origin")
		_, owner, _, ok := gitinfo.RemoteOwnerRepo(origin)
		if ok && orgs[strings.ToLower(owner)] {
			out = append(out, top)
		}
	}
	return out
}

func lower(l []string) []string {
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = strings.ToLower(s)
	}
	return out
}

func appendNew(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// placeBinary copies the running binary to bin (atomically), unless it is already there.
func placeBinary(bin string, dry bool) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	if rb, err := filepath.EvalSymlinks(bin); err == nil && rb == self {
		return bin + " (already this binary)", nil
	}
	if dry {
		return "would copy " + self + " to " + bin, nil
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	if cur, err := os.ReadFile(bin); err == nil && string(cur) == string(raw) {
		return bin + " (already up to date)", nil
	}
	// Not writeFileAtomic: a symlink here is replaced, never written through to its target.
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	tmp := bin + ".livy-tmp"
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, bin); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return "installed " + bin, nil
}

func readSettings(path string) (*object, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newObject(), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return newObject(), false, nil
	}
	o, err := parseObject(raw)
	return o, false, err
}

// writeConfig merges the profile file and the flags into the user config, keeping every
// other key.
func writeConfig(path string, o options, dry bool) (string, error) {
	cfg, err := readConfig(path)
	if err != nil {
		return "", err
	}
	before := encode(cfg)
	if o.profile != "" {
		raw, err := os.ReadFile(o.profile)
		if err != nil {
			return "", err
		}
		prof, err := parseObject(raw)
		if err != nil {
			return "", fmt.Errorf("%s: %v", o.profile, err)
		}
		deepMerge(cfg, prof)
	}
	settingFlags(cfg, o)
	switch {
	case string(before) == string(encode(cfg)):
		return path + " already holds these settings", nil
	case dry:
		return "would update " + path, nil
	}
	if err := writeFileAtomic(path, encode(cfg), 0o600); err != nil {
		return "", err
	}
	return "updated " + path, nil
}

// readConfig is the user config as an ordered object, empty when there is none.
func readConfig(path string) (*object, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s cannot be read (%w); not changed", path, err)
	}
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		return newObject(), nil
	}
	cfg, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is not usable (%w); not changed", path, err)
	}
	return cfg, nil
}

// settingFlags merges each install flag that sets a config value into cfg: the OTEL
// destination, the sink identity, scope and handle, and trailers.
func settingFlags(cfg *object, o options) {
	if o.otelTo != "" && !o.otelDefault {
		otel := newObject()
		switch o.otelTo {
		case "sink", otelNone:
			otel.set("destination", o.otelTo)
		default:
			otel.set("destination", "collector")
			otel.set("endpoint", o.otelTo)
		}
		patch := newObject()
		patch.set("otel", otel)
		deepMerge(cfg, patch)
	}
	if o.account != "" || o.writerSA != "" {
		sink := newObject()
		if o.account != "" {
			sink.set("account", o.account)
		}
		if o.writerSA != "" {
			sink.set("writer_sa", o.writerSA)
		}
		patch := newObject()
		patch.set("sink", sink)
		deepMerge(cfg, patch)
	}
	if len(o.orgs) > 0 || o.handle != "" {
		patch := newObject()
		if len(o.orgs) > 0 {
			scope := newObject()
			list := make([]any, len(o.orgs))
			for i, s := range o.orgs {
				list[i] = s
			}
			scope.set("orgs", list)
			patch.set("scope", scope)
		}
		if o.handle != "" {
			id := newObject()
			id.set("handle", o.handle)
			patch.set("identity", id)
		}
		deepMerge(cfg, patch)
	}
	if o.noTrailers {
		trailers := newObject()
		trailers.set("enabled", false)
		patch := newObject()
		patch.set("trailers", trailers)
		deepMerge(cfg, patch)
	}
}

func deepMerge(dst, src *object) {
	for _, k := range src.keys {
		sv := src.m[k]
		if so, ok := sv.(*object); ok {
			if do, ok := dst.m[k].(*object); ok {
				deepMerge(do, so)
				continue
			}
		}
		dst.set(k, clone(sv))
	}
}

// loadState is the recorded state, or the zero State when there is none or it is unusable.
// Readers that only report use it; Install uses readState, which says the file is corrupt.
func loadState(path string) State {
	st, _ := readState(path)
	return st
}

// readState is loadState with an error for a file that exists and is not a usable state record:
// unreadable, empty or not JSON.
func readState(path string) (State, error) {
	var st State
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return st, errors.New("the file is empty")
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

func saveState(path string, st State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeFileAtomic(path, append(b, '\n'), 0o600)
}

func guessSchedule(home string) Schedule {
	if runtime.GOOS == "darwin" {
		return Schedule{Kind: "launchd", Files: []string{filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")}}
	}
	dir := systemdUserDir(home)
	svc := filepath.Join(dir, systemdUnit+".service")
	if _, err := os.Stat(svc); err == nil {
		return Schedule{Kind: "systemd", Files: []string{svc, filepath.Join(dir, systemdUnit+".timer")}}
	}
	return Schedule{Kind: "cron"}
}

// writeFileAtomic writes through one fixed temp name beside path and renames it in place.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // a dotfiles symlink stays a symlink; its target is written
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".livy-tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Recorded is what the last install wrote, and the Claude Code settings file it used.
func Recorded() (State, string) {
	p := where()
	st := loadState(p.statePath)
	if st.Settings != "" {
		return st, st.Settings
	}
	return st, p.settings
}

// OurBinary is the Livy binary a hook command runs, or "" when the command is not ours.
func OurBinary(cmd string) string {
	if w := firstWord(cmd); strings.HasSuffix(w, "/.livy/bin/livy") {
		return w
	}
	return ""
}

// localSinkFolder is the folder, in the home folder, a machine with no upload destination keeps
// its data in.
const localSinkFolder = "livy-data"

// makeSinkFolder makes the default sink folder owner-only. A folder already there is used as it
// is, never re-moded: it may be the person's own; a link there is refused.
func makeSinkFolder(dir string) error {
	switch st, err := os.Lstat(dir); {
	case err == nil && st.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a link; set sink.path to the folder you mean", dir)
	case err == nil && !st.IsDir():
		return fmt.Errorf("%s is not a folder; set sink.path to the folder you mean", dir)
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("could not read %s: %w", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("could not make %s: %w", dir, err)
	}
	return nil
}

// isLocalDefault is whether a config's sink section is exactly the local folder ensureSink
// records, and nothing else.
func isLocalDefault(sink *object, home string) bool {
	t, _ := sink.m["type"].(string)
	path, _ := sink.m["path"].(string)
	return len(sink.m) == 2 && t == "local" && path == filepath.Join(home, localSinkFolder)
}

// ensureSink records a local sink at ~/livy-data, made 0700, when no layer sets a sink: without
// one, everything stays in the data folder, where the OTEL stream fills its cap. A sink any layer
// sets, a half-set one included, is left to decide. It says what it did ("" for nothing).
func ensureSink(home string, dry bool, profile string) (string, error) {
	if _, err := emit.FromSettings(hook.Settings()); !errors.Is(err, emit.ErrNoSink) {
		return "", nil
	}
	if profile != "" {
		// A dry run has not merged the profile yet: its sink is the one this install sets.
		if p, err := readConfig(profile); err == nil && p.m["sink"] != nil {
			return "", nil
		}
	}
	dir := filepath.Join(home, localSinkFolder)
	if dry {
		return "would keep data in a local sink folder, " + dir + " (no upload destination is set)", nil
	}
	path := hook.UserConfigPath()
	cfg, err := readConfig(path)
	if err != nil {
		return "", err
	}
	if err := makeSinkFolder(dir); err != nil {
		return "", err
	}
	sink := newObject()
	sink.set("type", "local")
	sink.set("path", dir)
	patch := newObject()
	patch.set("sink", sink)
	deepMerge(cfg, patch)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("could not make %s: %w", filepath.Dir(path), err)
	}
	if err := writeFileAtomic(path, encode(cfg), 0o600); err != nil {
		return "", err
	}
	return "no upload destination is set: data goes to a local sink folder, " + dir + " (sink.type local in " + path + ")", nil
}

func sameFolder(a, b string) (bool, error) {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb, nil
}
