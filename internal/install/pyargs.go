package install

import "strings"

// Arguments the earlier Python installer took, accepted so a script or habit written for it
// keeps working. Each maps onto the Livy flag that does the same job; the ones with no Livy
// counterpart (an identity denylist, a built-in OTEL endpoint) are not accepted.

// pyAlias maps a Python flag to the Livy flag with the same meaning.
var pyAlias = map[string]string{
	"--no-launch-agent": "--no-schedule",
	"--otel-endpoint":   "--otel",
}

// pyOtelDest are the flags that name where OTEL goes (`--flag V` or `--flag=V`). Livy turns
// OTEL on only when one of them is given.
var pyOtelDest = map[string]bool{"--otel-endpoint": true, "--otel": true, "--otel-to": true}

// pyFlag splits an argument the way Go's flag package reads it: one or two leading dashes,
// then the name, then an optional `=value`. name is "--name" whichever dash form was typed,
// and "" when a is not a flag.
func pyFlag(a string) (name, value string, hasValue bool) {
	if len(a) < 2 || a[0] != '-' {
		return "", "", false
	}
	n := strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
	n, value, hasValue = strings.Cut(n, "=")
	return "--" + n, value, hasValue
}

// translatePyArgs rewrites Python-style arguments to Livy's. `--orgs a b c` becomes
// `--org a --org b --org c`; `--account` and `--writer-sa` stay and are read as the
// sink's identity (see writeConfig).
//
// The two OTEL opt-outs follow the Python installer:
//   - `--no-otel` wins over a destination: the destination flag and its value are dropped,
//     and it becomes `--otel-to none`, so OTEL is off (Livy turns its receiver on by default).
//   - `--no-codex-otel` keeps Claude Code's export and drops Codex's, which Livy's own flag
//     of the same name does.
func translatePyArgs(args []string) []string {
	noOtel := false
	for _, a := range args {
		if name, _, _ := pyFlag(a); name == "--no-otel" {
			noOtel = true
		}
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := pyFlag(a)
		switch {
		case name == "--orgs" && hasValue:
			out = append(out, "--org", value)
		case a == "--orgs":
			for i+1 < len(args) && len(args[i+1]) > 0 && args[i+1][0] != '-' {
				out = append(out, "--org", args[i+1])
				i++
			}
		case name == "--no-otel":
			out = append(out, "--otel-to", otelNone) // OTEL is on by default: off has to be said
		case name == "--no-codex-otel":
			if hasValue {
				out = append(out, "--no-codex-otel="+value) // Go's flag package reads the explicit value
			} else {
				out = append(out, "--no-codex-otel")
			}
		case pyOtelDest[name]:
			var dest []string
			dest, i = pyOtelArgs(args, i, noOtel)
			out = append(out, dest...)
		case pyAlias[a] != "":
			out = append(out, pyAlias[a])
		default:
			out = append(out, a)
		}
	}
	return out
}

// pyOtelArgs is Livy's form of a Python OTEL destination flag at args[i], and the index of
// the last word it read (its value may be the next word).
func pyOtelArgs(args []string, i int, noOtel bool) ([]string, int) {
	name, value, hasValue := pyFlag(args[i])
	if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
		value, hasValue = args[i+1], true
		i++
	}
	switch {
	case noOtel:
		return nil, i // --no-otel wins: neither the flag nor its value reaches Livy
	case !hasValue:
		return []string{pyAliasOf(name)}, i // no value: Livy's own "needs an argument" error
	}
	return []string{pyAliasOf(name), value}, i
}

// pyAliasOf is the Livy name for a Python flag, or the flag itself when it has no alias.
func pyAliasOf(flag string) string {
	if to := pyAlias[flag]; to != "" {
		return to
	}
	return flag
}
