package hook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/0xbew/livy/internal/gitinfo"
	"github.com/0xbew/livy/internal/pytext"
)

// Selftest proves a write round-trips on this machine, and is the one path allowed to
// fail: a self-test that always passes tests nothing. It feeds itself an empty stdin
// rather than inheriting one, so its result never depends on how it was launched.
func Selftest(stdout, stderr io.Writer) int {
	tmp, err := os.MkdirTemp("", "livy-selftest-")
	if err != nil {
		fmt.Fprintf(stderr, "selftest FAILED: no temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "ledger.jsonl")
	// Scope is bypassed for the writes and checked on its own below: the install doc runs
	// the self-test from wherever the engineer happens to be.
	for _, args := range [][]string{{"session.started", "session_id=abc"}, {"not.a.real.event"},
		{"work.started", "branch=feature/x"}} {
		r := &run{stderr: stderr, git: gitinfo.New(stderr), path: path}
		r.handle(args, nil, false)
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "selftest: ledger unreadable: %v\n", err)
		return 1
	}
	records, failures := readSelftestLedger(f)
	_ = f.Close()
	failures = append(failures, ledgerFailures(records)...)
	scope, scopeFailed := scopeFailures(stderr)
	failures = append(failures, scopeFailed...)
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(stderr, "selftest FAILED: %s\n", f)
		}
		return 1
	}
	fmt.Fprintf(stdout, "selftest ok: 3 events, required fields present, UTC timestamps, scope = %s\n",
		strings.Join(scope, ", "))
	return 0
}

// readSelftestLedger is the JSON records the self-test wrote, and a failure for each line
// that is an empty object or not JSON.
func readSelftestLedger(f io.Reader) ([]map[string]any, []string) {
	var records []map[string]any
	var failures []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ln := sc.Text()
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if strings.TrimSpace(ln) == "{}" {
			failures = append(failures, "wrote an empty object line")
		} else if err := json.Unmarshal([]byte(ln), &m); err != nil {
			failures = append(failures, fmt.Sprintf("wrote a line that is not JSON: %q", ln[:min(60, len(ln))]))
		} else {
			records = append(records, m)
		}
	}
	return records, failures
}

// ledgerFailures checks the written records: the right events, an unknown name turned into a
// ledger.error, the required fields, and UTC timestamps.
func ledgerFailures(records []map[string]any) []string {
	var failures []string
	has := func(ev string) bool {
		for _, r := range records {
			if r["event"] == ev {
				return true
			}
		}
		return false
	}
	if !has("session.started") {
		failures = append(failures, "session.started was not written")
	}
	if !has("work.started") {
		failures = append(failures, "work.started was not written")
	}
	if has("not.a.real.event") {
		failures = append(failures, "an unknown event name was written as-is")
	}
	if !has("ledger.error") {
		failures = append(failures, "an unknown event name did not produce a ledger.error")
	}
	for _, r := range records {
		for _, k := range []string{"schema_version", "ts", "event", "repo"} {
			if !pytext.Truthy(r[k]) {
				failures = append(failures, fmt.Sprintf("record missing required %s: %v", k, r["event"]))
			}
		}
		if ts, _ := r["ts"].(string); !strings.HasSuffix(ts, "+00:00") && !strings.HasSuffix(ts, "Z") {
			failures = append(failures, fmt.Sprintf("timestamp is not timezone-aware UTC: %q", ts))
		}
	}
	return failures
}

// scopeFailures checks the configured scope accepts in-scope remotes and refuses others,
// and returns the in-scope orgs with the failures.
func scopeFailures(stderr io.Writer) ([]string, []string) {
	scope := orgs(stderr)
	g := gitinfo.New(stderr)
	g.Hosts = hosts(stderr)
	switch {
	case g.Hosts != nil && len(g.Hosts) == 0:
		return scope, []string{"scope.hosts names no host"}
	case len(scope) == 0:
		return scope, []string{"no in-scope orgs configured"}
	}
	in := func(url string) bool { return g.InScope(url, func() []string { return orgs(stderr) }) }
	host := gitinfo.ScopeHost
	if len(g.Hosts) > 0 {
		host = strings.ToLower(g.Hosts[0])
	}
	other := outOfScopeHost(g.Hosts)
	var failures []string
	org := scope[0]
	for _, u := range []string{"git@" + host + ":" + org + "/r.git", "https://" + host + "/" + org + "/r",
		"ssh://git@" + host + "/" + org + "/r.git"} {
		if !in(u) {
			failures = append(failures, "an in-scope remote was rejected: "+u)
		}
	}
	for _, u := range []string{"git@" + host + ":someone-else/r.git", "https://" + other + "/" + org + "/r", ""} {
		if in(u) {
			failures = append(failures, fmt.Sprintf("an out-of-scope remote was accepted: %q", u))
		}
	}
	return scope, failures
}

// outOfScopeHost is a well-known host no configured entry names.
func outOfScopeHost(configured []string) string {
	for _, h := range []string{"gitlab.com", gitinfo.ScopeHost, "bitbucket.org"} {
		allowed := configured == nil && h == gitinfo.ScopeHost
		for _, c := range configured {
			allowed = allowed || strings.EqualFold(strings.TrimSpace(c), h)
		}
		if !allowed {
			return h
		}
	}
	return "not-in-scope.invalid"
}
