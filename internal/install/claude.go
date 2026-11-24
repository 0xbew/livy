package install

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/0xbew/livy/internal/ccpolicy"
	"github.com/0xbew/livy/internal/hook"
	"github.com/0xbew/livy/internal/relay"
)

// throttleSeconds is the mid-session upload's minimum gap (contract rule 7).
const throttleSeconds = "300"

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string { return shellQuote(s) }

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '+' || r == ':' || r == '@' ||
			('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isOurs says whether a hook entry runs the installed binary: an entry whose command runs
// <anything>/.livy/bin/livy, whatever the home folder, so a second install under another
// HOME still recognises the first one's entries.
func isOurs(entry any) bool {
	o, ok := entry.(*object)
	if !ok {
		return false
	}
	cmd, _ := o.m["command"].(string)
	return strings.HasSuffix(firstWord(cmd), "/.livy/bin/livy")
}

// firstWord is the first word of a POSIX shell command, quotes removed.
func firstWord(cmd string) string {
	var b strings.Builder
	quote := byte(0)
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(cmd) {
				i++
				b.WriteByte(cmd[i])
			} else {
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(cmd):
			i++
			b.WriteByte(cmd[i])
		case c == ' ' || c == '\t':
			if b.Len() > 0 {
				return b.String()
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func entry(bin, args string, timeout int, async bool) *object {
	o := newObject()
	o.set("type", "command")
	o.set("command", shellQuote(bin)+" "+args)
	if timeout > 0 {
		o.set("timeout", timeout)
	}
	if async {
		o.set("async", true)
	}
	return o
}

func group(matcher string, entries ...*object) *object {
	g := newObject()
	if matcher != "" {
		g.set("matcher", matcher)
	}
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	g.set("hooks", list)
	return g
}

// desiredHooks are the Claude Code hook groups install adds, by event. The mid-session
// upload (transcript extraction, then the sweep, in one process) is async and throttled,
// so a tool call pays one stat() when it is not yet due. SessionStart also starts an
// extraction pass, as the Python tool does; its counts are dropped, since an async hook's
// output is attached to the session (its failures are ledger.error lines anyway).
// SessionEnd has one hook: the uploader writes session.ended itself and then sweeps, so
// the session's last line ships with its own segment.
func desiredHooks(bin string) map[string][]*object {
	hook := func(event string) *object { return entry(bin, "hook "+event, 5, false) }
	flush := func() *object { return entry(bin, "emit --extract --throttle "+throttleSeconds, 0, true) }
	pr := hook("work.pr_opened")
	pr.set("if", "Bash(gh pr create *)")
	return map[string][]*object{
		"SessionStart":     {group("startup|resume|clear|fork", hook("session.started"), entry(bin, "extract >/dev/null 2>&1", 0, true))},
		"UserPromptSubmit": {group("", hook("prompt.submitted"), hook("skill.loaded"), flush())},
		// Spot-checks are log-only and async: Claude Code never waits on the network, and
		// without a Jev key the hook records nothing.
		"PreToolUse":         {group("Bash|Agent|Task", entry(bin, "spotcheck", 30, true))},
		"PostToolUse":        {group("Bash", pr), group("Skill", hook("skill.loaded")), group("", flush())},
		"PostModelSwitch":    {group("", hook("model.switched"))},
		"InstructionsLoaded": {group("", hook("instructions.loaded"))},
		"Stop":               {group("", hook("turn.ended"), flush())},
		"SessionEnd":         {group("", entry(bin, "emit --extract --event session.ended", 0, true))},
	}
}

// mergeHooks puts our groups into settings, replacing only entries isOurs recognises and
// leaving every other group, entry and key as it was. An event whose value is not a list,
// or a hooks value that is not an object, is someone else's shape: refused and named.
func mergeHooks(settings *object, wanted map[string][]*object) (refused []string) {
	raw, ok := settings.get("hooks")
	if !ok || raw == nil {
		raw = newObject()
		settings.set("hooks", raw)
	}
	hooks, ok := raw.(*object)
	if !ok {
		return sortedKeys(wanted)
	}
	for _, event := range sortedKeys(wanted) {
		existing, present := hooks.get(event)
		list, isList := existing.([]any)
		if present && existing != nil && !isList {
			refused = append(refused, event)
			continue
		}
		kept := withoutOurs(list)
		for _, g := range wanted[event] {
			kept = append(kept, g)
		}
		hooks.set(event, kept)
	}
	return refused
}

// stripHooks removes our entries everywhere in settings, dropping a group left empty by
// that and an event left with no group; foreign ones are untouched.
func stripHooks(settings *object) {
	raw, _ := settings.get("hooks")
	hooks, ok := raw.(*object)
	if !ok {
		return
	}
	for _, event := range append([]string(nil), hooks.keys...) {
		list, ok := hooks.m[event].([]any)
		if !ok {
			continue
		}
		kept := withoutOurs(list)
		if len(kept) == 0 && len(list) > 0 {
			hooks.del(event)
		} else {
			hooks.set(event, kept)
		}
	}
	if len(hooks.keys) == 0 {
		settings.del("hooks")
	}
}

// withoutOurs drops our entries from each group; a group that held only ours goes, and a
// group of someone else's (even an empty one) stays as it was.
func withoutOurs(list []any) []any {
	kept := []any{}
	for _, g := range list {
		gobj, ok := g.(*object)
		if !ok {
			kept = append(kept, g)
			continue
		}
		entries, ok := gobj.m["hooks"].([]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		var others []any
		ours := 0
		for _, e := range entries {
			if isOurs(e) {
				ours++
			} else {
				others = append(others, e)
			}
		}
		switch {
		case ours == 0:
			kept = append(kept, g)
		case len(others) > 0:
			c := clone(gobj).(*object)
			c.set("hooks", others)
			kept = append(kept, c)
		}
	}
	return kept
}

// setOwned sets each wanted key of section (an object inside settings) when the key is
// absent or still holds the value an earlier install set; a key holding anything else is
// someone's own and is left, and named in skipped (unless it already holds the wanted
// value, which is theirs and stays theirs). owned is what this install set.
func setOwned(settings *object, section string, wanted, previous map[string]string) (owned map[string]string, skipped []string) {
	owned = map[string]string{}
	raw, ok := settings.get(section)
	if !ok {
		raw = newObject()
	}
	sec, isObj := raw.(*object)
	if !isObj {
		return owned, []string{section + " (not an object)"}
	}
	for _, k := range sortedKeys(wanted) {
		cur, present := sec.get(k)
		s, isStr := cur.(string)
		prev, hadPrev := previous[k]
		if present && !(isStr && hadPrev && s == prev) {
			if !isStr || s != wanted[k] {
				skipped = append(skipped, fmt.Sprintf("%s.%s (holds a value install did not set)", section, k))
			}
			continue
		}
		sec.set(k, wanted[k])
		owned[k] = wanted[k]
	}
	if len(sec.keys) > 0 {
		settings.set(section, sec)
	}
	return owned, skipped
}

// unsetOwned removes each key that still holds the value install set.
func unsetOwned(settings *object, section string, owned map[string]string) {
	raw, _ := settings.get(section)
	sec, ok := raw.(*object)
	if !ok {
		return
	}
	for k, v := range owned {
		if cur, _ := sec.m[k].(string); cur == v {
			sec.del(k)
		}
	}
	if len(sec.keys) == 0 {
		settings.del(section)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// claudeOtelUnderPolicy sets the OTEL keys install owns, unless Claude Code's managed settings
// override Livy's route: then it changes no OTEL key and says why, or with --otel-around-managed
// writes only the per-signal traces and logs keys the managed settings leave unset.
func (r *installRun) claudeOtelUnderPolicy(settings *object, next *State) {
	m, err := ccpolicy.Read(r.p.managed)
	if err != nil {
		r.say("otel: Claude Code's managed settings could not be read (%v); livy doctor checks them again", err)
	}
	var conflict []string
	for _, o := range m.Overrides() {
		if o.Breaks {
			conflict = append(conflict, o.Key)
		}
	}
	if len(conflict) == 0 {
		if r.o.aroundManaged {
			r.say("otel: no managed setting overrides Livy's route; --otel-around-managed writes nothing more")
		}
		r.claudeOtel(settings, next, nil)
		unsetKeyHeaders(settings, r.prev.RelayKeyEnv)
		next.RelayKeyEnv = nil
		return
	}
	r.say("otel: Claude Code's managed settings (%s) set %s, which win over Livy's: Claude Code's OTEL does not reach Livy here (livy doctor says why)",
		strings.Join(m.Files, ", "), strings.Join(conflict, ", "))
	if !r.o.aroundManaged {
		r.say("otel: Claude Code's OTEL settings left as they are; `livy install --otel-around-managed` writes the per-signal keys the managed settings leave unset")
		return
	}
	unset, blocked := m.PerSignal()
	if blocked != "" {
		r.fail("otel: no route around the managed settings: %s; nothing written", blocked)
		return
	}
	r.claudeOtel(settings, next, &m)
	r.keyHeaders(settings, next, unset)
}

// keyHeaders points each per-signal endpoint in unset at Livy's receiver and sets each
// per-signal headers key in unset to the relay key, which Claude Code merges with the
// managed helper's headers.
func (r *installRun) keyHeaders(settings *object, next *State, unset []string) {
	want, names := map[string]string{}, []string(nil)
	for _, k := range unset {
		sig, isEndpoint := strings.CutSuffix(strings.TrimPrefix(k, "OTEL_EXPORTER_OTLP_"), "_ENDPOINT")
		if isEndpoint {
			want[k] = "http://" + RelayListen + "/v1/" + strings.ToLower(sig)
			continue
		}
		names = append(names, k)
	}
	key := relay.ReadKey(hook.DataDir())
	if key == "" {
		r.fail("otel: no relay key in %s yet, so no per-signal key is written; re-run livy install --otel-around-managed", hook.DataDir())
		return
	}
	env, skipped := setOwned(settings, "env", want, r.prev.Env)
	maps.Copy(next.Env, env)
	// The key goes only beside an endpoint install set to Livy's receiver: a signal whose
	// endpoint holds someone else's value would carry it there.
	names = slices.DeleteFunc(names, func(k string) bool {
		_, ours := env[strings.TrimSuffix(k, "_HEADERS")+"_ENDPOINT"]
		return !ours
	})
	// A header an earlier run wrote for a signal it no longer routes.
	unsetKeyHeaders(settings, slices.DeleteFunc(slices.Clone(r.prev.RelayKeyEnv), func(k string) bool { return slices.Contains(names, k) }))
	owned, held := setKeyHeaders(settings, names, key)
	next.RelayKeyEnv = owned
	for _, s := range append(skipped, held...) {
		r.say("otel: %s left alone", s)
	}
	// An endpoint to Livy whose header holds someone else's value would only be refused.
	for _, k := range names {
		if ep := strings.TrimSuffix(k, "_HEADERS") + "_ENDPOINT"; !slices.Contains(owned, k) {
			unsetOwned(settings, "env", map[string]string{ep: env[ep]})
			delete(next.Env, ep)
		}
	}
	if len(owned) == 0 {
		r.fail("otel: no per-signal route written around the managed settings: each signal's keys hold values install did not set")
		return
	}
	r.say("otel: per-signal relay-key headers written around the managed settings: %s (the relay key is now in %s; "+
		"Claude Code versions that guard a managed headers helper drop these endpoints too)", strings.Join(owned, ", "), r.p.settings)
}

// keyHeaderValue is an OTLP headers value carrying the relay key.
func keyHeaderValue(key string) string { return relay.KeyHeader + "=" + key }

// setKeyHeaders sets each name in env to the relay key header where it is unset or already
// holds one; it returns the names it set and the ones holding something else.
func setKeyHeaders(settings *object, names []string, key string) (owned, held []string) {
	raw, ok := settings.get("env")
	sec, isObj := raw.(*object)
	if !ok {
		sec, isObj = newObject(), true
	}
	if !isObj {
		return nil, []string{"env (not an object)"}
	}
	for _, k := range names {
		cur, present := sec.get(k)
		if s, isStr := cur.(string); present && (!isStr || !strings.HasPrefix(s, relay.KeyHeader+"=")) {
			held = append(held, "env."+k+" (holds a value install did not set)")
			continue
		}
		sec.set(k, keyHeaderValue(key))
		owned = append(owned, k)
	}
	if len(sec.keys) > 0 {
		settings.set("env", sec)
	}
	return owned, held
}

// unsetKeyHeaders removes each name that still holds a relay key header.
func unsetKeyHeaders(settings *object, names []string) {
	raw, _ := settings.get("env")
	sec, ok := raw.(*object)
	if !ok {
		return
	}
	for _, k := range names {
		if s, _ := sec.m[k].(string); strings.HasPrefix(s, relay.KeyHeader+"=") {
			sec.del(k)
		}
	}
	if len(sec.keys) == 0 {
		settings.del("env")
	}
}
