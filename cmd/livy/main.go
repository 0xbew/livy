// Command livy is the per-machine capture tool. Subcommands land one phase at a time
// (DESIGN §1); the conformance harness reports a case whose subcommand is missing as PENDING.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/0xbew/livy/internal/amp"
	"github.com/0xbew/livy/internal/denylist"
	"github.com/0xbew/livy/internal/doctor"
	"github.com/0xbew/livy/internal/emit"
	"github.com/0xbew/livy/internal/extract/cli"
	"github.com/0xbew/livy/internal/githook"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/incident"
	"github.com/0xbew/livy/internal/install"
	"github.com/0xbew/livy/internal/profile"
	"github.com/0xbew/livy/internal/prompts"
	"github.com/0xbew/livy/internal/relay"
	"github.com/0xbew/livy/internal/show"
	"github.com/0xbew/livy/internal/spotcheck"
	"github.com/0xbew/livy/internal/why"
)

// implemented lists the subcommands advertised to the conformance harness. The ones left out
// (relay, ...) are built but graded by the outcome contract, not byte parity with the Python
// tool, so their cases stay pending. extract and emit are listed: the harness compares each by
// value (conformance/harness/extractview.py, emitview.py), as the contract does.
var implemented = []string{"hook", "git post-checkout", "extract", "spotcheck", "prompt-capture", "install", "uninstall", "doctor", "emit", "amp"}

func sinkFor() (emit.Sink, error) { return emit.FromSettings(hook.Settings()) }

// doctorStatus handles emit's --doctor-status PATH (the daily job): run doctor, write its
// status file, and record its verdict as one doctor.status event before the sweep, so it
// ships now. It returns the other arguments. It never stops the upload.
func doctorStatus(ctx context.Context, args []string) (rest []string) {
	const flag = "--doctor-status"
	path := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag && i+1 < len(args):
			path = args[i+1]
			i++
		case strings.HasPrefix(args[i], flag+"="):
			path = strings.TrimPrefix(args[i], flag+"=")
		default:
			rest = append(rest, args[i])
		}
	}
	if path == "" {
		return rest
	}
	for _, a := range rest {
		if a == "--dry-run" {
			return rest // a dry run writes nothing, the status file and event included
		}
	}
	defer func() {
		if r := recover(); r != nil {
			hook.WriteError(fmt.Sprintf("doctor: panic while writing the status: %v", r), os.Stderr)
		}
	}()
	// Before doctor, so its denylist row reads this refresh. Not in the exit status: a failed
	// refresh keeps the copy, and doctor's denylist row says so.
	_ = denylist.Main(ctx, io.Discard, io.Discard)
	rep := doctor.Check(ctx, sinkFor)
	if ctx.Err() != nil {
		return rest // stopped: the rows say "context canceled", which is not the machine's state
	}
	if err := doctor.WriteStatus(path, rep); err != nil {
		hook.WriteError("doctor: could not write the status file: "+err.Error(), os.Stderr)
	}
	if !hook.Append(doctor.Status(rep), os.Stderr) {
		hook.WriteError("emit: could not write doctor.status", os.Stderr)
	}
	return rest
}

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// withSignals runs f with a context that SIGINT and SIGTERM (Ctrl-C, launchd stopping the daily
// job) cancel. Only the subcommands that do network or process work take it: a hook must stay
// killable the way it always was.
// stopGrace is how long work in flight may take to wind down after SIGINT or SIGTERM.
const stopGrace = 5 * time.Second

func withSignals(f func(ctx context.Context) int) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Steps that do not watch the context (a lock wait, extraction) must not outlive a stop request.
		time.Sleep(stopGrace)
		os.Exit(1)
	}()
	return f(ctx)
}

// dispatch runs one subcommand and returns the process's exit status.
func dispatch(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "__conformance-subcommands":
		for _, s := range implemented {
			fmt.Println(s)
		}
		return 0
	case len(args) >= 1 && args[0] == "extract":
		signal.Ignore(syscall.SIGPIPE)
		return cli.Main(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "vault":
		signal.Ignore(syscall.SIGPIPE)
		return prompts.VaultMain(args[1:], cli.Roots(nil), os.Stdout, os.Stderr)
	case len(args) >= 1 && (args[0] == "prompts" || args[0] == "prompt-capture"):
		signal.Ignore(syscall.SIGPIPE)
		return prompts.Main(args[1:], cli.Roots(nil), os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "hook":
		// A host that closes the hook's stderr early must not turn a diagnostic into a
		// SIGPIPE death: the hook exits 0 whatever happens to its pipes.
		signal.Ignore(syscall.SIGPIPE)
		if len(args) == 2 && args[1] == "--selftest" {
			return hook.Selftest(os.Stdout, os.Stderr)
		}
		return hook.Run(args[1:], os.Stdin, os.Stderr)
	case len(args) >= 1 && args[0] == "amp":
		// Run by the Amp plugin, which reads any exit but 0 as a crash: 0 whatever happens to its pipes.
		signal.Ignore(syscall.SIGPIPE)
		return amp.Main(args[1:], os.Stdin, os.Stderr)
	case len(args) >= 1 && args[0] == "spotcheck":
		// The hook exits 0 whatever happens to its pipes, and prints nothing a host could read.
		signal.Ignore(syscall.SIGPIPE)
		return spotcheck.Main(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case len(args) >= 2 && args[0] == "git" && args[1] == "post-checkout":
		signal.Ignore(syscall.SIGPIPE)
		return githook.PostCheckout(args[2:])
	case len(args) >= 2 && args[0] == "git" && args[1] == "prepare-commit-msg":
		// Go-only: no Python reference, so it is not in `implemented` for the harness.
		signal.Ignore(syscall.SIGPIPE)
		return githook.PrepareCommitMsg(args[2:])
	case len(args) >= 2 && args[0] == "git" && args[1] == "post-commit":
		// Go-only, like prepare-commit-msg.
		signal.Ignore(syscall.SIGPIPE)
		return githook.PostCommit(args[2:])
	case len(args) >= 2 && args[0] == "git" && args[1] == "post-rewrite":
		signal.Ignore(syscall.SIGPIPE)
		return githook.PostRewrite(args[2:], os.Stdin)
	case len(args) >= 1 && args[0] == "emit":
		signal.Ignore(syscall.SIGPIPE)
		return withSignals(func(ctx context.Context) int {
			return emit.Run(ctx, doctorStatus(ctx, args[1:]), os.Stdin, os.Stderr, sinkFor)
		})
	default:
		return machineCommand(args)
	}
}

// The usage line, either side of the old recorder's subcommands.
const (
	usageHead = "usage: livy <subcommand> [args]: hook <event> | hook --selftest | amp <event> | spotcheck [--selftest] | git post-checkout <prev> <new> <flag> | git prepare-commit-msg <file> [source] [sha] | git post-commit | git post-rewrite amend|rebase | emit [--extract] [--throttle S] [--event NAME] [--dry-run] [--doctor-status P] | install [--refresh-denylist] | uninstall | doctor [--json] [--status-file P]"
	usageTail = " | prompts [--upload on|off] [--backfill-days N] [--dry-run] | why [--data DIR] <sha|A..B> | profile [--top N] [--all] [--data DIR] <sha|A..B> | show --prompt <id> | --commit <sha> | incident <id> --range A..B [--data DIR] [--out FILE] [--excerpts] | vault backfill [--days N] | relay (--collector URL [--headers-command CMD] | --sink) --listen 127.0.0.1:PORT | version"
)

// oldRecorderCommands are the subcommands that run beside or take over from the Python
// recorder, by name, and oldRecorderUsage their part of the usage line. The private build
// adds them (oldrecorder.go, which the public export leaves out); the public build has none.
var (
	oldRecorderCommands = map[string]func(args []string) int{}
	oldRecorderUsage    string
)

// machineCommand runs the subcommands that set up or check a machine; dispatch tries the
// capture commands first.
func machineCommand(args []string) int {
	switch {
	case len(args) >= 1 && args[0] == "doctor":
		return withSignals(func(ctx context.Context) int {
			return doctor.Run(ctx, args[1:], os.Stdout, os.Stderr, sinkFor)
		})
	case len(args) >= 1 && args[0] == "install" && slices.Contains(args[1:], "--refresh-denylist"):
		if len(args) != 2 {
			// It takes no other flag: a --dry-run beside it would otherwise be ignored and the copy replaced.
			fmt.Fprintln(os.Stderr, "livy install --refresh-denylist takes no other arguments")
			return 2
		}
		return withSignals(func(ctx context.Context) int { return denylist.Main(ctx, os.Stdout, os.Stderr) })
	case len(args) >= 1 && args[0] == "install":
		return install.Install(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && oldRecorderCommands[args[0]] != nil:
		return oldRecorderCommands[args[0]](args[1:])
	case len(args) >= 1 && args[0] == "uninstall":
		return install.Uninstall(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "why":
		return why.Main(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "profile":
		return profile.Main(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "show":
		return show.Main(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "incident":
		return incident.Main(args[1:], os.Stdout, os.Stderr)
	case len(args) >= 1 && args[0] == "relay":
		return relay.Main(args[1:], os.Stderr)
	case len(args) >= 1 && args[0] == "otel-headers":
		return install.OtelHeaders(args[1:], os.Stdout, os.Stderr)
	case len(args) == 1 && (args[0] == "version" || args[0] == "--version"):
		return printVersion(os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usageHead+oldRecorderUsage+usageTail)
		return 2
	}
}
