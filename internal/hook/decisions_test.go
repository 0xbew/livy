package hook

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/0xbew/livy/internal/gitinfo"
)

// Every expected value is the Python ledger's own answer (testdata/gen.py), so it can
// disagree with this code.
func TestDecisionsMatchPython(t *testing.T) {
	// Guards: a false or missed work.pr_opened (an invented or dropped work boundary), a
	// remote slugged differently from CI's (repo never joins), a pr_opened credited to the
	// session's branch when the command worked in another checkout.
	if n := checkPythonRows(t, "testdata/python_decisions.jsonl"); n < 100 {
		t.Fatalf("only %d rows", n)
	}
}

// Guards: a PR created after a quoted $(...), on a continued line or after a comment going
// unrecorded, and quoted text after a substitution counting as a PR. Expected values are
// Python's (testdata/gen.py), or Go's where Python's is a bug (go_overrides.json).
func TestPrCreateReadsShellFormsAsTheShellDoes(t *testing.T) {
	if n := checkPythonRows(t, "testdata/python_shell_forms.jsonl"); n == 0 {
		t.Fatal("no rows")
	}
}

// checkPythonRows checks every row of a Python-generated decisions file and returns the count.
func checkPythonRows(t *testing.T, file string) int {
	t.Helper()
	t.Setenv("HOME", "/home/dev")
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Where Python's answer is a bug, Go's answer is in go_overrides.json, by class.
	var overrides map[string]struct{ Rows map[string]bool }
	if raw, err := os.ReadFile("testdata/go_overrides.json"); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(raw, &overrides); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		var row struct {
			Fn  string
			In  json.RawMessage
			Out any
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		var got any
		switch row.Fn {
		case "runs_gh_pr_create":
			var in string
			_ = json.Unmarshal(row.In, &in)
			got = RunsGhPrCreate(in)
			for class, o := range overrides {
				if want, ok := o.Rows[in]; ok {
					if want == row.Out {
						t.Errorf("%s: %q: Python already answers %v; not a Python bug", class, in, want)
					}
					row.Out = want
				}
			}
		case "remote_owner_repo":
			var in string
			_ = json.Unmarshal(row.In, &in)
			if h, o, r, ok := gitinfo.RemoteOwnerRepo(in); ok {
				got = []any{h, o, r}
			}
		case "leading_directory":
			var in string
			_ = json.Unmarshal(row.In, &in)
			if v, d, ok := LeadingDirectory(in); ok {
				got = []any{v, d}
			}
		case "named_directory":
			var in [3]any
			_ = json.Unmarshal(row.In, &in)
			cwd, _ := in[2].(string)
			tool, _ := in[0].(string)
			if v, d, ok := NamedDirectory(tool, in[1], cwd); ok {
				got = []any{v, d}
			}
		default:
			t.Fatalf("unknown fn %q", row.Fn)
		}
		if !reflect.DeepEqual(got, row.Out) {
			t.Errorf("%s(%s): got %#v, want %#v", row.Fn, row.In, got, row.Out)
		}
	}
	return n
}

// Guards: the quoted text left after a substitution closes inside double quotes being read
// as a command, which invents a work.pr_opened. Expected values are the Python ledger's
// answers for the same inputs (it gets these right).
func TestQuotedTextAfterASubstitutionIsNotACommand(t *testing.T) {
	for in, want := range map[string]bool{
		`echo "$(date) gh pr create"`:               false,
		"git commit -m \"x $(date)\ngh pr create\"": false,
		`git commit -m "$(date)" && gh pr create`:   true,
	} {
		if got := RunsGhPrCreate(in); got != want {
			t.Errorf("RunsGhPrCreate(%q) = %v, want %v", in, got, want)
		}
	}
}

// Guards: the ) of a subshell inside "$( ... )" restoring the double quote early, so a
// real `gh pr create` after it is read as quoted text and work.pr_opened is lost.
func TestSubshellInsideAQuotedSubstitutionKeepsItOpen(t *testing.T) {
	for in, want := range map[string]bool{
		`echo "$( (date); gh pr create )"`: true,
		`echo "$( (date) ) gh pr create"`:  false,
	} {
		if got := RunsGhPrCreate(in); got != want {
			t.Errorf("RunsGhPrCreate(%q) = %v, want %v", in, got, want)
		}
	}
}
