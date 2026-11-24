// Package ccpolicy reads the keys of Claude Code's managed settings, the tier an administrator
// sets, that decide whether Livy records Claude Code: its OTEL route and its hooks. Claude
// Code applies them over every user setting key by key, and some take Livy's OTEL route
// away: a managed otelHeadersHelper replaces Livy's (so the export never carries the relay
// key), a managed endpoint or protocol replaces Livy's, and Claude Code drops a user's own
// endpoint or per-signal protocol when a managed key claims it.
// Only the managed files are read: settings pushed by an MDM profile or by the server are
// not visible here.
package ccpolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Dir is the folder Claude Code reads managed-settings.json and managed-settings.d from.
func Dir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	}
	return "/etc/claude-code"
}

// Settings is the part of the managed settings that decides where Claude Code's OTEL goes,
// and whether Livy's hooks run at all.
type Settings struct {
	Files  []string          // the files read, in the order Claude Code applies them
	Helper string            // otelHeadersHelper, "" when unset
	Env    map[string]string // the env block, keys upper-cased, a later file's value winning
	// ManagedHooksOnly is allowManagedHooksOnly, which keeps Livy's user hooks from running.
	ManagedHooksOnly bool
	// NoHooks is disableAllHooks, nil when the managed settings leave it to the user settings.
	NoHooks *bool
}

type document struct {
	Helper           *string        `json:"otelHeadersHelper"`
	Env              map[string]any `json:"env"`
	ManagedHooksOnly *bool          `json:"allowManagedHooksOnly"`
	NoHooks          *bool          `json:"disableAllHooks"`
}

// Read reads dir's managed-settings.json, then each managed-settings.d/*.json in name order,
// a later file's keys over an earlier one's, as Claude Code merges them. Absent files are no
// settings; a file that cannot be read or parsed is an error naming it.
func Read(dir string) (Settings, error) {
	s := Settings{Env: map[string]string{}}
	paths := []string{filepath.Join(dir, "managed-settings.json")}
	dropIns, err := os.ReadDir(filepath.Join(dir, "managed-settings.d"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("read %s: %w", filepath.Join(dir, "managed-settings.d"), err)
	}
	names := make([]string, 0, len(dropIns))
	for _, e := range dropIns {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	for _, n := range names {
		paths = append(paths, filepath.Join(dir, "managed-settings.d", n))
	}
	for _, p := range paths {
		if err := s.apply(p); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s *Settings) apply(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var d document
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	s.Files = append(s.Files, path)
	if d.Helper != nil {
		s.Helper = strings.TrimSpace(*d.Helper)
	}
	if d.ManagedHooksOnly != nil {
		s.ManagedHooksOnly = *d.ManagedHooksOnly
	}
	if d.NoHooks != nil {
		s.NoHooks = d.NoHooks
	}
	// Claude Code reads a managed key in any case as the upper-case one, which wins over
	// another spelling in the same file: upper-case keys go last.
	upper := func(k string) bool { return k == strings.ToUpper(k) }
	keys := slices.SortedFunc(maps.Keys(d.Env), func(a, b string) int {
		if ua := upper(a); ua != upper(b) {
			if ua {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	for _, k := range keys {
		if v := d.Env[k]; v != nil {
			// Claude Code puts a settings env value into the process env as text.
			s.Env[strings.ToUpper(k)] = fmt.Sprint(v)
		}
	}
	return nil
}

// Override is one managed key that changes where, or how, Claude Code exports.
type Override struct {
	Key string
	// Breaks is whether it takes Livy's route away for traces or logs; false is a key
	// Livy can live with, said for the record.
	Breaks bool
	Why    string
}

const prefix = "OTEL_EXPORTER_OTLP_"

// HookBlocks are the managed keys that keep Livy's user hooks from running.
func (s Settings) HookBlocks() []Override {
	var out []Override
	if s.ManagedHooksOnly {
		out = append(out, Override{"allowManagedHooksOnly", true, "Claude Code runs only the managed settings' hooks, so " +
			"Livy's user hooks never run and the ledger records no Claude Code session"})
	}
	if s.NoHooks != nil && *s.NoHooks {
		out = append(out, Override{"disableAllHooks", true, "Claude Code runs no hooks at all, so the ledger records no Claude Code session"})
	}
	return out
}

// Overrides are the managed keys that override Livy's OTEL route, most damaging first.
func (s Settings) Overrides() []Override {
	var out []Override
	if v, ok := s.Env["CLAUDE_CODE_ENABLE_TELEMETRY"]; ok && !on(v) {
		out = append(out, Override{"CLAUDE_CODE_ENABLE_TELEMETRY", true, "Claude Code exports no OTEL at all (" + v + ")"})
	}
	if s.Helper != "" {
		out = append(out, Override{"otelHeadersHelper", true, "Claude Code sends that helper's headers instead of Livy's, " +
			"so no export carries Livy's relay key or collector token and each is refused; Claude Code versions that guard a " +
			"managed helper also drop every endpoint set outside the managed settings"})
	}
	for _, k := range []string{"ENDPOINT", "HEADERS", "CLIENT_KEY", "CLIENT_CERTIFICATE"} {
		if s.set(prefix + k) {
			out = append(out, Override{prefix + k, true,
				"Claude Code drops every OTEL endpoint set outside the managed settings, so all signals go where the managed settings say"})
		}
	}
	for _, sig := range []string{"TRACES", "LOGS"} {
		if s.set(prefix + sig + "_ENDPOINT") {
			out = append(out, Override{prefix + sig + "_ENDPOINT", true,
				strings.ToLower(sig) + " go to the managed endpoint, not to Livy"})
		}
		if v := s.Env["OTEL_"+sig+"_EXPORTER"]; v != "" && !strings.Contains(v, "otlp") {
			out = append(out, Override{"OTEL_" + sig + "_EXPORTER", true, strings.ToLower(sig) + " are not exported (" + v + ")"})
		}
	}
	out = append(out, s.protocolOverrides()...)
	if s.set(prefix + "METRICS_ENDPOINT") {
		out = append(out, Override{prefix + "METRICS_ENDPOINT", false, "metrics go to the managed endpoint by design; Livy reads spans and logs, not metrics"})
	} else if k := s.metricsProtocolKey(); s.Env[k] == "http/protobuf" {
		out = append(out, Override{k, false, "metrics arrive at Livy's receiver as protobuf, which it refuses and counts as not stored; spans and logs are unaffected"})
	}
	return out
}

// metricsProtocolKey is the managed key that decides the metrics protocol.
func (s Settings) metricsProtocolKey() string {
	if s.set(prefix + "METRICS_PROTOCOL") {
		return prefix + "METRICS_PROTOCOL"
	}
	return prefix + "PROTOCOL"
}

// protocolOverrides: a managed protocol wins, and Claude Code drops a per-signal one set
// outside the managed settings. Each of traces and logs goes out in its managed per-signal
// protocol, else the managed generic one. Livy's receiver takes their OTLP over HTTP as JSON
// or protobuf, as does a collector install; gRPC never reaches either.
func (s Settings) protocolOverrides() []Override {
	var out []Override
	judge := func(k, what string) {
		if v := s.Env[k]; v != "" && v != "http/json" && v != "http/protobuf" {
			out = append(out, Override{k, true, what + " go out as " + v + ", which Livy's OTLP/HTTP receiver does not take"})
		}
	}
	var fallback []string
	for _, sig := range []string{"TRACES", "LOGS"} {
		if k := prefix + sig + "_PROTOCOL"; s.Env[k] != "" {
			judge(k, strings.ToLower(sig))
		} else {
			fallback = append(fallback, strings.ToLower(sig))
		}
	}
	if len(fallback) > 0 {
		judge(prefix+"PROTOCOL", strings.Join(fallback, " and "))
	}
	return out
}

// PerSignal is what a route around the managed settings can be: the per-signal endpoint and
// header keys for traces and logs the managed settings leave unset, or why there is none. A
// per-signal header survives a managed headers helper (Claude Code merges the two; the
// helper wins only on the same name), so of the managed keys that break Livy's route only
// the helper, and a signal the managed settings take for themselves, leave a route.
func (s Settings) PerSignal() (unset []string, blocked string) {
	taken := map[string]bool{}
	for _, o := range s.Overrides() {
		sig, perSignal := signalOf(o.Key)
		switch {
		case !o.Breaks || o.Key == "otelHeadersHelper":
		case perSignal:
			taken[sig] = true // its endpoint, exporter or protocol is the managed settings'
		default:
			return nil, o.Key + " in the managed settings: " + o.Why
		}
	}
	for _, sig := range []string{"TRACES", "LOGS"} {
		if taken[sig] || s.set(prefix+sig+"_HEADERS") {
			continue
		}
		unset = append(unset, prefix+sig+"_ENDPOINT", prefix+sig+"_HEADERS")
	}
	if len(unset) == 0 {
		return nil, "the managed settings decide both traces and logs themselves"
	}
	return unset, ""
}

// signalOf is the signal a per-signal key names: TRACES or LOGS.
func signalOf(key string) (string, bool) {
	for _, sig := range []string{"TRACES", "LOGS"} {
		if strings.HasPrefix(key, prefix+sig+"_") || key == "OTEL_"+sig+"_EXPORTER" {
			return sig, true
		}
	}
	return "", false
}

// on is how Claude Code reads a boolean env value.
func on(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s Settings) set(key string) bool { return strings.TrimSpace(s.Env[key]) != "" }
