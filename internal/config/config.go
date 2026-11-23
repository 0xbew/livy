// Package config resolves Livy's settings from four layers, highest first: managed
// (an admin's file users cannot override), env (LIVY_*, plus the old names in
// package oldnames), user (~/.livy/config.json), and built-in defaults. A key
// the managed layer sets is locked, and a managed key under sink locks the whole sink
// section: a lower layer that tries to change a locked key is ignored and named. DESIGN §3.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/0xbew/livy/internal/oldnames"
)

// Layer names, highest precedence first.
const (
	Managed  = "managed"
	Env      = "env"
	User     = "user"
	Defaults = "defaults"
)

// ErrDuplicateKey marks a layer file that repeats a key; the whole file is refused.
var ErrDuplicateKey = errors.New("duplicate key")

// Problem is a layer file that was refused; the remaining layers still apply.
type Problem struct {
	Layer string
	Err   error
}

// Inputs are the raw layers: file contents (nil when there is no file), the process
// environment, and the built-in defaults.
type Inputs struct {
	Managed, User []byte
	// ManagedErr is why a managed file that is there could not be read; nil when it was
	// read or there is none.
	ManagedErr error
	Environ    []string
	Defaults   map[string]any
}

// Load flattens each file, refusing a malformed one as a whole, and resolves the layers.
// A refused file contributes nothing; the caller reports each Refused entry (doctor, and
// a ledger.error from the hooks, which still exit 0). A managed file that is there and is
// refused (unreadable or malformed) may lock any key, so while it is, no lower layer sets a
// scope, sink or otel key: ManagedUnusable is set and those keys are held closed.
func Load(in Inputs) Resolved {
	var refused []Problem
	if in.ManagedErr != nil {
		refused = append(refused, Problem{Managed, in.ManagedErr})
	}
	file := func(name string, raw []byte) map[string]any {
		if raw == nil {
			return nil
		}
		m, err := Flatten(raw)
		if err != nil {
			refused = append(refused, Problem{name, err})
			return nil
		}
		return m
	}
	managed := file(Managed, in.Managed)
	user := file(User, in.User)
	unusable := slices.ContainsFunc(refused, func(p Problem) bool { return p.Layer == Managed })
	env := FromEnv(in.Environ)
	var held []Override
	if unusable {
		user, held = holdClosed(user, User, held)
		env, held = holdClosed(env, Env, held)
	}
	out := Resolve(managed, env, user, in.Defaults)
	out.Locked = append(out.Locked, held...)
	sortOverrides(out.Locked)
	out.Refused = refused
	out.ManagedUnusable = unusable
	return out
}

// unusableManaged is the Lock of a key held back because the managed file is refused.
const unusableManaged = "(refused managed file)"

// holdClosed drops the scope, sink and otel keys from one lower layer's values, recording each
// as an Override: a refused managed file may have locked any of them.
func holdClosed(values map[string]any, layer string, held []Override) (map[string]any, []Override) {
	kept := map[string]any{}
	for k, v := range values {
		if strings.HasPrefix(k, scopeSection+".") || strings.HasPrefix(k, otelSection+".") || lockedSection(k) != "" {
			held = append(held, Override{k, layer, unusableManaged})
			continue
		}
		kept[k] = v
	}
	return kept, held
}

// String is the line doctor prints for a refused file.
func (p Problem) String() string {
	if p.Layer == Managed {
		return fmt.Sprintf("managed config file refused (%v); until it is fixed no repository is in scope, nothing "+
			"is uploaded and no OTEL is exported, as it may lock scope, sink and otel; other settings come from env, "+
			"user and defaults", p.Err)
	}
	return fmt.Sprintf("%s config file refused (%v); settings come from managed, env and defaults", p.Layer, p.Err)
}

// ManagedRefusal is the line for a refused managed file, "" when it is not refused.
func (r Resolved) ManagedRefusal() string {
	for _, p := range r.Refused {
		if p.Layer == Managed {
			return p.String()
		}
	}
	return ""
}

// String is the line doctor prints for a refused key.
func (o Override) String() string {
	if o.Lock == unusableManaged {
		return fmt.Sprintf("%s sets %s; the managed file is refused and may lock it, so it is ignored", o.Layer, o.Key)
	}
	if o.Lock != o.Key {
		return fmt.Sprintf("%s sets %s; the managed file locks the %s section, so it is ignored", o.Layer, o.Key, o.Lock)
	}
	return fmt.Sprintf("%s sets %s; the managed file sets it, so it is ignored", o.Layer, o.Key)
}

// Setting is a key's winning value and the layer it came from.
type Setting struct {
	Value any // string, bool, json.Number or []string
	Layer string
}

// Override is a lower layer's attempt to change a key the managed layer locked.
type Override struct {
	Key   string
	Layer string
	Lock  string // the managed key or section that refused it
}

// Resolved is the result of merging the layers.
type Resolved struct {
	Settings map[string]Setting
	Locked   []Override // sorted by key, then layer
	Refused  []Problem  // layer files refused as a whole, managed first
	// ManagedUnusable is a managed file that is there and refused: scope, sink and otel keys
	// are held closed, so a caller must not fall back to another source for them.
	ManagedUnusable bool
}

// envKeys are the keys the env layer can set, and whether each is a comma-separated list.
var envKeys = map[string]bool{
	"scope.hosts": true, "scope.orgs": true, "scope.mode": false,
	"identity.handle": false, "identity.command": false, "content": false,
	"sink.type": false, "sink.bucket": false, "sink.writer_sa": false,
	"otel.endpoint": false, "otel.destination": false, "denylist.path": false, "denylist.command": false, "ticket.regex": false,
}

// legacyEnv maps the Python tool's variable names to keys; the LIVY_ and old Livy names win.
var legacyEnv = map[string]string{
	"scope.orgs":     oldnames.RecorderEnv("ORGS"),
	"sink.writer_sa": oldnames.RecorderEnv("WRITER_SA"),
	"sink.bucket":    oldnames.RecorderEnv("BUCKET"),
}

// EnvName is the LIVY_ variable that sets key.
func EnvName(key string) string { return "LIVY_" + envSuffix(key) }

// OldEnvName is the name Livy answered to for key before the rename; "" when it had none.
func OldEnvName(key string) string { return oldnames.LivyEnv(envSuffix(key)) }

func envSuffix(key string) string { return strings.ToUpper(strings.ReplaceAll(key, ".", "_")) }

// sinkSection is locked as a whole when the managed layer sets any key in it: no lower
// layer may redirect where data goes.
const sinkSection = "sink"

// scopeSection is what is recorded: held closed with the sinks while the managed file is refused.
const scopeSection = "scope"

// otelSection is where OTEL is exported: held closed with the sinks while the managed file is
// refused.
const otelSection = "otel"

// sinkSections are the sections the managed layer locks whole: the ledger's sink and the
// prompt text's.
var sinkSections = []string{sinkSection, "prompts.sink"}

// lockedSection is the sink section k belongs to, "" for any other key.
func lockedSection(k string) string {
	for _, s := range sinkSections {
		if strings.HasPrefix(k, s+".") {
			return s
		}
	}
	return ""
}

// Resolve merges flattened layers (dotted key -> value) by precedence.
func Resolve(managed, env, user, defaults map[string]any) Resolved {
	sinkLocked := map[string]bool{}
	for k := range managed {
		if s := lockedSection(k); s != "" {
			sinkLocked[s] = true
		}
	}
	out := Resolved{Settings: map[string]Setting{}}
	for k, v := range defaults {
		out.Settings[k] = Setting{v, Defaults}
	}
	for _, l := range []struct {
		name   string
		values map[string]any
	}{{User, user}, {Env, env}} {
		for k, v := range l.values {
			out.layerValue(k, v, l.name, managed, sinkLocked)
		}
	}
	for k, v := range managed {
		out.Settings[k] = Setting{v, Managed}
	}
	sortOverrides(out.Locked)
	return out
}

// sortOverrides orders overrides by key, then layer.
func sortOverrides(o []Override) {
	sort.Slice(o, func(i, j int) bool {
		a, b := o[i], o[j]
		return a.Key < b.Key || (a.Key == b.Key && a.Layer < b.Layer)
	})
}

// layerValue sets one user or env value, or records it as overridden when the managed file
// sets that key or locks its section.
func (out *Resolved) layerValue(k string, v any, layer string, managed map[string]any, sinkLocked map[string]bool) {
	if mv, ok := managed[k]; ok {
		if !reflect.DeepEqual(v, mv) {
			out.Locked = append(out.Locked, Override{k, layer, k})
		}
		return
	}
	if lock := lockingSection(k, sinkLocked); lock != "" {
		out.Locked = append(out.Locked, Override{k, layer, lock})
		return
	}
	out.Settings[k] = Setting{v, layer}
}

// lockingSection is the managed section that locks key k, or "".
func lockingSection(k string, sinkLocked map[string]bool) string {
	s := lockedSection(k)
	if s == "" {
		return ""
	}
	// The policy that says where the ledger goes also says where prompt text goes.
	if !sinkLocked[s] && (s != "prompts.sink" || !sinkLocked[sinkSection]) {
		return ""
	}
	if !sinkLocked[s] {
		return sinkSection
	}
	return s
}

// Flatten turns a layer file's JSON object into dotted keys: {"scope":{"orgs":["a"]}}
// becomes scope.orgs = ["a"]. Arrays are values; the top level must be an object.
func Flatten(raw []byte) (map[string]any, error) {
	if err := checkDuplicates(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top any
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: data after the object")
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return nil, errors.New("config: the file must hold a JSON object")
	}
	out := map[string]any{}
	return out, flatten("", obj, out)
}

func flatten(prefix string, obj map[string]any, out map[string]any) error {
	for k, v := range obj {
		if k == "" || strings.Contains(k, ".") {
			return fmt.Errorf("config: key %q: names are nested objects, never dotted", prefix+k)
		}
		key := prefix + k
		switch x := v.(type) {
		case map[string]any:
			if err := flatten(key+".", x, out); err != nil {
				return err
			}
		case []any:
			list := make([]string, 0, len(x))
			for i, e := range x {
				s, ok := e.(string)
				if !ok {
					return fmt.Errorf("config: %s[%d]: want a string", key, i)
				}
				list = append(list, s)
			}
			out[key] = list
		case string, bool, json.Number:
			out[key] = x
		default:
			return fmt.Errorf("config: %s: null is not a setting; leave the key out", key)
		}
	}
	return nil
}

// checkDuplicates walks the tokens and fails on a key repeated within one object, which
// a plain decode would silently resolve last-wins. It reads numbers as Flatten does, and
// a token it cannot read refuses the file: a check that stopped early would let every
// later repeat through.
func checkDuplicates(raw []byte) error {
	var stack dupStack
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		if err := stack.token(tok); err != nil {
			return err
		}
	}
}

// dupFrame is one open object or array: the keys an object has seen, and whether its next
// token is a key.
type dupFrame struct {
	obj       bool
	keys      map[string]bool
	expectKey bool
}

// dupStack is the open objects and arrays, innermost last.
type dupStack []*dupFrame

// token takes the next token; an error is a key repeated in its object.
func (st *dupStack) token(tok json.Token) error {
	var top *dupFrame
	if len(*st) > 0 {
		top = (*st)[len(*st)-1]
	}
	if t, ok := tok.(json.Delim); ok {
		switch t {
		case '{', '[':
			if top != nil && top.obj {
				top.expectKey = true
			}
			*st = append(*st, &dupFrame{obj: t == '{', keys: map[string]bool{}, expectKey: true})
		default:
			*st = (*st)[:len(*st)-1]
		}
		return nil
	}
	if top == nil || !top.obj {
		return nil
	}
	if !top.expectKey {
		top.expectKey = true
		return nil
	}
	k, _ := tok.(string)
	if top.keys[k] {
		return fmt.Errorf("%w %q", ErrDuplicateKey, k)
	}
	top.keys[k] = true
	top.expectKey = false
	return nil
}

// FromEnv reads the env layer from KEY=VALUE pairs (os.Environ's form).
func FromEnv(environ []string) map[string]any {
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	out := map[string]any{}
	for key, isList := range envKeys {
		for _, name := range []string{EnvName(key), OldEnvName(key), legacyEnv[key]} {
			if v, ok := envValue(vars[name], isList); name != "" && ok {
				out[key] = v
				break
			}
		}
	}
	return out
}

// envValue parses one variable; a blank value, or a list naming nothing, is unset.
func envValue(raw string, isList bool) (any, bool) {
	if !isList {
		v := strings.TrimSpace(raw)
		return v, v != ""
	}
	var list []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			list = append(list, p)
		}
	}
	return list, len(list) > 0
}

// OldNames lists the old Livy name's variables that are set and actually used: the key has
// no LIVY_ value and the managed file does not decide it, so the old one decides. Sorted.
func OldNames(environ []string, managed []byte) []string {
	vars := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	resolved := Load(Inputs{Managed: managed, Environ: environ})
	var out []string
	for key, isList := range envKeys {
		if resolved.Settings[key].Layer != Env {
			continue
		}
		if _, ok := envValue(vars[EnvName(key)], isList); ok {
			continue
		}
		if _, ok := envValue(vars[OldEnvName(key)], isList); ok && OldEnvName(key) != "" {
			out = append(out, OldEnvName(key))
		}
	}
	sort.Strings(out)
	return out
}
