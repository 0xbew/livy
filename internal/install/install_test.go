package install

import "testing"

// Guards: an install that rewrites or drops the engineer's own hooks, permissions or env
// (rule 22), a re-install that adds a second set of hooks (every event then written twice),
// and an uninstall that leaves ours behind or takes theirs (rule 23). The expected value is
// the engineer's original file.
func TestInstallThenUninstallLeavesOnlyTheEngineersSettings(t *testing.T) {
	original := `{
  "permissions": {"allow": ["Bash(ls)"], "defaultMode": "auto"},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "echo mine"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "~/bin/guard", "timeout": 30}]}]
  },
  "env": {"FOO": "1", "OTEL_LOGS_EXPORTER": "console"},
  "model": "opus"
}`
	orig, err := parseObject([]byte(original))
	if err != nil {
		t.Fatal(err)
	}
	doc := clone(orig).(*object)
	bin := "/home/eng/.livy/bin/livy"
	want := map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}
	mergeHooks(doc, desiredHooks(bin))
	owned, skipped := setOwned(doc, "env", want, nil)
	once := encode(doc)
	mergeHooks(doc, desiredHooks(bin))
	setOwned(doc, "env", want, owned)
	if string(encode(doc)) != string(once) {
		t.Errorf("a second install changed the file:\n%s", encode(doc))
	}
	if len(skipped) != 1 || owned["OTEL_LOGS_EXPORTER"] != "" {
		t.Errorf("the engineer's OTEL_LOGS_EXPORTER was taken over: owned %v, skipped %v", owned, skipped)
	}
	stripHooks(doc)
	unsetOwned(doc, "env", owned)
	if !same(doc, orig) {
		t.Errorf("after uninstall:\n%s\nwant the original:\n%s", encode(doc), encode(orig))
	}
}

// Guards: Codex's config.toml losing a table Codex wrote, or ours taking over a foreign
// [otel] table. The expected value is the engineer's original text.
func TestCodexTableRoundTrip(t *testing.T) {
	original := "model = \"gpt-6\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"
	with := withCodexBlock(original, codexBlock("http://collector:4318", "", "", "eng", signals{true, true, true}))
	if codexHasForeignOtel(with) {
		t.Errorf("our own [otel] table read as foreign:\n%s", with)
	}
	later := with + "\n[projects.\"/y\"]\ntrust_level = \"trusted\"\n" // Codex appends after ours
	if got := withoutCodexBlock(later); got != original+"\n[projects.\"/y\"]\ntrust_level = \"trusted\"\n" {
		t.Errorf("after removal:\n%q", got)
	}
	if !codexHasForeignOtel(original + "\n[otel]\nenvironment = \"mine\"\n") {
		t.Error("a foreign [otel] table was not seen")
	}
}
