package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/0xbew/livy/internal/oldnames"
)

// Expected values are DESIGN §3's rules as decided 2026-11-03: managed > env > user >
// defaults, a managed key is locked, and a lower layer that tries to change it is named.
func TestResolvePrecedenceAndLocks(t *testing.T) {
	// Guards: a user or env value overriding an admin's managed setting, or winning silently.
	managed := map[string]any{"scope.orgs": []string{"acme"}, "content": "metadata"}
	env := map[string]any{"content": "labels", "identity.handle": "octocat", "ticket.regex": "E-\\d+"}
	user := map[string]any{"scope.orgs": []string{"acme"}, "ticket.regex": "U-\\d+", "identity.command": "id"}
	defaults := map[string]any{"ticket.regex": "[A-Z][A-Z0-9]+-\\d+", "scope.mode": "org"}
	got := Resolve(managed, env, user, defaults)
	want := map[string]Setting{
		"scope.orgs":       {[]string{"acme"}, Managed},
		"content":          {"metadata", Managed},
		"identity.handle":  {"octocat", Env},
		"ticket.regex":     {"E-\\d+", Env},
		"identity.command": {"id", User},
		"scope.mode":       {"org", Defaults},
	}
	if !reflect.DeepEqual(got.Settings, want) {
		t.Errorf("settings:\n got %v\nwant %v", got.Settings, want)
	}
	// The user repeating the managed orgs changes nothing, so it is not an override.
	wantLocked := []Override{{"content", Env, "content"}}
	if !reflect.DeepEqual(got.Locked, wantLocked) {
		t.Errorf("locked: got %v, want %v", got.Locked, wantLocked)
	}
}

// A managed key anywhere under sink locks the whole sink section: no lower layer may
// redirect where data goes, even through a sink key the managed file does not set.
func TestManagedSinkLocksTheWholeSection(t *testing.T) {
	// Guards: a user pointing a managed install's uploads at their own bucket or endpoint.
	managed := map[string]any{"sink.type": "gcs"}
	env := map[string]any{"sink.bucket": "mine", "otel.endpoint": "https://e.example.test"}
	user := map[string]any{"sink.type": "gcs", "sink.writer_sa": "me@example.test", "sink.dir": "/tmp/x"}
	got := Resolve(managed, env, user, map[string]any{"sink.dir": "/var/livy"})
	want := map[string]Setting{
		"sink.type":     {"gcs", Managed},
		"sink.dir":      {"/var/livy", Defaults}, // defaults are ours, never an attempt to redirect
		"otel.endpoint": {"https://e.example.test", Env},
	}
	if !reflect.DeepEqual(got.Settings, want) {
		t.Errorf("settings:\n got %v\nwant %v", got.Settings, want)
	}
	wantLocked := []Override{{"sink.bucket", Env, "sink"}, {"sink.dir", User, "sink"}, {"sink.writer_sa", User, "sink"}}
	if !reflect.DeepEqual(got.Locked, wantLocked) {
		t.Errorf("locked: got %v, want %v", got.Locked, wantLocked)
	}
}

// A file with a repeated key is refused as a whole, never read last-wins, and the other
// layers still apply.
func TestDuplicateKeysRefuseTheWholeFile(t *testing.T) {
	// Guards: {"sink":{"type":"gcs"},"sink":{}} silently dropping a managed lock.
	for _, dup := range []string{`{"content":"a","content":"b"}`, `{"sink":{"type":"gcs"},"sink":{}}`,
		`{"scope":{"orgs":["a"],"orgs":["b"]}}`} {
		if _, err := Flatten([]byte(dup)); !errors.Is(err, ErrDuplicateKey) {
			t.Errorf("%s: got %v, want ErrDuplicateKey", dup, err)
		}
	}
	got := Load(Inputs{
		Managed:  []byte(`{"sink":{"type":"gcs"},"sink":{}}`),
		User:     []byte(`{"content":"labels","content":"metadata"}`),
		Environ:  []string{"LIVY_SINK_TYPE=local"},
		Defaults: map[string]any{"content": "metadata"},
	})
	// The refused managed file may have locked the sink, so the env sink is held back too.
	want := map[string]Setting{"content": {"metadata", Defaults}}
	if !reflect.DeepEqual(got.Settings, want) {
		t.Errorf("settings:\n got %v\nwant %v", got.Settings, want)
	}
	var refused []string
	for _, p := range got.Refused {
		if errors.Is(p.Err, ErrDuplicateKey) {
			refused = append(refused, p.Layer)
		}
	}
	if !reflect.DeepEqual(refused, []string{Managed, User}) {
		t.Errorf("refused layers: got %v (%v), want [managed user]", refused, got.Refused)
	}
}

// The duplicate check reads every number the decode accepts, so one it cannot hold as a
// float64 does not end the check early and let later repeats through last-wins.
func TestDuplicateKeyAfterAHugeNumberIsStillRefused(t *testing.T) {
	// Guards: {"budget":1e400,"sink":{"type":"gcs"},"sink":{}} dropping the managed sink lock.
	for _, dup := range []string{`{"budget":1e400,"sink":{"type":"gcs"},"sink":{}}`,
		`{"spotcheck":{"budget":-1e999},"content":"a","content":"b"}`} {
		if _, err := Flatten([]byte(dup)); !errors.Is(err, ErrDuplicateKey) {
			t.Errorf("%s: got %v, want ErrDuplicateKey", dup, err)
		}
	}
}

func TestFlattenLayerFile(t *testing.T) {
	// Guards: a nested managed setting not recognised as the key it locks, or a malformed file read as empty.
	got, err := Flatten([]byte(`{"scope":{"orgs":["acme","beta"],"mode":"org"},"content":"labels","otel":{"auth":{"command":"x"}},"spotcheck":{"enabled":true,"budget":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"scope.orgs": []string{"acme", "beta"}, "scope.mode": "org", "content": "labels",
		"otel.auth.command": "x", "spotcheck.enabled": true, "spotcheck.budget": json.Number("3"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	for _, bad := range []string{`["a"]`, `{"scope":`, `{"scope":{"orgs":[1]}}`, `{"a.b":"x"}`, `{"content":null}`} {
		if _, err := Flatten([]byte(bad)); err == nil {
			t.Errorf("%s: flattened without error", bad)
		}
	}
}

// oldVar is name=value for an old variable name; a build with no old names skips the test.
func oldVar(t *testing.T, name, value string) string {
	t.Helper()
	if name == "" {
		t.Skip("this build has no old names")
	}
	return name + "=" + value
}

func TestFromEnvReadsNewAndLegacyNames(t *testing.T) {
	// Guards: an engineer's existing Python-tool ORGS variable being dropped at the switch, or a blank value clearing a setting.
	sp, py := oldnames.LivyEnv, oldnames.RecorderEnv
	got := FromEnv([]string{
		oldVar(t, sp("SCOPE_ORGS"), "acme, beta,,"),
		oldVar(t, py("ORGS"), "legacy"),
		oldVar(t, sp("CONTENT"), "labels"),
		oldVar(t, py("WRITER_SA"), "w@example.test"),
		oldVar(t, sp("IDENTITY_HANDLE"), ""),
		oldVar(t, sp("NOT_A_KEY"), "x"),
		"HOME=/home/x",
	})
	want := map[string]any{
		"scope.orgs":     []string{"acme", "beta"},
		"content":        "labels",
		"sink.writer_sa": "w@example.test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := FromEnv([]string{oldVar(t, py("ORGS"), "Old-Org")}); !reflect.DeepEqual(got, map[string]any{"scope.orgs": []string{"Old-Org"}}) {
		t.Errorf("legacy alone: got %v", got)
	}
	if got := FromEnv([]string{oldVar(t, py("ORGS"), " , ")}); len(got) != 0 {
		t.Errorf("an orgs list naming no org must be ignored, got %v", got)
	}
}

func TestLivyNameWinsAndOldNameIsReportedOnlyWhenItDecides(t *testing.T) {
	// Guards: the rename making an existing old-name setting stop working, or the new name losing to the old one.
	sp := oldnames.LivyEnv
	environ := []string{
		"LIVY_CONTENT=labels", oldVar(t, sp("CONTENT"), "metadata"), // both set: new wins, nothing to report
		oldVar(t, sp("SCOPE_ORGS"), "acme"), // old only: read, and reported
		oldVar(t, sp("NOT_A_KEY"), "x"),
		oldVar(t, oldnames.RecorderEnv("BUCKET"), "b"),
	}
	got := FromEnv(environ)
	want := map[string]any{"content": "labels", "scope.orgs": []string{"acme"}, "sink.bucket": "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if names := OldNames(environ, nil); !reflect.DeepEqual(names, []string{sp("SCOPE_ORGS")}) {
		t.Errorf("OldNames = %v, want [%s]", names, sp("SCOPE_ORGS"))
	}
}

func TestOldNameOverriddenByManagedIsNotReported(t *testing.T) {
	// Guards: a ledger.error naming the old SCOPE_ORGS variable as in use when the admin's managed file pins scope.orgs and the variable is ignored.
	sp := oldnames.LivyEnv
	environ := []string{oldVar(t, sp("SCOPE_ORGS"), "acme"), oldVar(t, sp("CONTENT"), "labels")}
	managed := []byte(`{"scope":{"orgs":["corp"]}}`)
	if names := OldNames(environ, managed); !reflect.DeepEqual(names, []string{sp("CONTENT")}) {
		t.Errorf("OldNames = %v, want [%s]: scope.orgs is decided by the managed file", names, sp("CONTENT"))
	}
}

// A managed sink also locks prompts.sink: the policy that says where the ledger goes must
// say where prompt text goes, and a user's own prompts.sink is ignored, not obeyed.
func TestManagedSinkLocksPromptsSinkToo(t *testing.T) {
	got := Resolve(map[string]any{"sink.type": "gcs"}, nil, map[string]any{"prompts.sink.type": "local"}, nil)
	if _, set := got.Settings["prompts.sink.type"]; set {
		t.Errorf("a user's prompts.sink was obeyed: %v", got.Settings)
	}
	if want := []Override{{"prompts.sink.type", User, "sink"}}; !reflect.DeepEqual(got.Locked, want) {
		t.Errorf("locked: got %v, want %v", got.Locked, want)
	}
}

// A managed file that is there but cannot be read or parsed may lock any key, so no lower
// layer may set scope, a sink or otel while it is refused; every other key still resolves.
func TestAnUnusableManagedFileHoldsScopeAndSinkClosed(t *testing.T) {
	// Guards: an admin's managed.json that fails to parse or read being dropped, so a user's or
	// the environment's scope.orgs widened what is recorded and their sink took the uploads, and
	// their otel.destination turned on an OTEL export the admin's file may have turned off.
	user := []byte(`{"scope":{"orgs":["mine"],"hosts":["git.example"]},"sink":{"type":"local","path":"/tmp/mine"},` +
		`"prompts":{"sink":{"type":"local","path":"/tmp/prompts"}},"otel":{"destination":"collector",` +
		`"endpoint":"https://mine.example"},"content":"labels"}`)
	env := []string{"LIVY_SCOPE_ORGS=envorg", "LIVY_SINK_BUCKET=envbucket", "LIVY_OTEL_DESTINATION=sink"}
	cases := []struct {
		name       string
		managed    []byte
		managedErr error
	}{
		{"cut short", []byte(`{"scope":`), nil},
		{"duplicate key", []byte(`{"scope":{"orgs":["acme"]},"scope":{}}`), nil},
		{"unreadable", nil, fs.ErrPermission},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Load(Inputs{Managed: c.managed, ManagedErr: c.managedErr, User: user, Environ: env})
			want := map[string]Setting{"content": {"labels", User}}
			if !reflect.DeepEqual(got.Settings, want) {
				t.Errorf("settings with the managed file refused:\n got %v\nwant %v", got.Settings, want)
			}
			if len(got.Refused) != 1 || got.Refused[0].Layer != Managed {
				t.Errorf("refused %v, want the managed file", got.Refused)
			}
		})
	}
	// Only the managed file holds scope and sinks closed: a refused user file leaves env's.
	got := Load(Inputs{User: []byte(`{"scope":`), Environ: env})
	if s := got.Settings["scope.orgs"]; !reflect.DeepEqual(s, Setting{[]string{"envorg"}, Env}) {
		t.Errorf("scope.orgs with only the user file refused = %v, want env's envorg", s)
	}
}
