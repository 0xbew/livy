package event

import (
	"bufio"
	"os"
	"testing"
)

// testdata/python_lines.jsonl holds lines the Python ledger encoder wrote: 25 sanitised
// shapes of real ledger lines (every key seen per event in 10,063 real lines, strings
// replaced) and 6 lines built for shapes the real sample lacks (nulls, doctor_checks,
// an integer score, non-ASCII), each encoded by Python's json.dumps.
func TestDecodeEncodeReproducesPythonLines(t *testing.T) {
	// Guards: a decoded-then-encoded ledger line whose bytes, and so its segment key, differ from Python's.
	f, err := os.Open("testdata/python_lines.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		ev, err := Decode([]byte(line))
		if err != nil {
			t.Errorf("line %d: decode: %v", n, err)
			continue
		}
		got, err := ev.Encode()
		if err != nil {
			t.Errorf("line %d: encode: %v", n, err)
			continue
		}
		if string(got) != line {
			t.Errorf("line %d:\n got %s\nwant %s", n, got, line)
		}
	}
	if n != 31 {
		t.Fatalf("read %d lines, want 31", n)
	}
}

func TestDecodeRefusesWhatTheSchemaForbids(t *testing.T) {
	// Guards: a malformed line read as a valid event (schema: additionalProperties false, required, types).
	ok := `"event":"turn.ended","repo":"x","schema_version":1,"ts":"2026-11-03T12:00:00+00:00"`
	rows := map[string]string{
		"unknown field":                  `{` + ok + `,"bogus":1}`,
		"float in an integer field":      `{` + ok + `,"raw_stdin_bytes":1.5}`,
		"number in a string field":       `{` + ok + `,"branch":5}`,
		"null in a non-nullable field":   `{` + ok + `,"branch":null}`,
		"string in a nullable number":    `{` + ok + `,"relay_lag_minutes":"3"}`,
		"missing required ts":            `{"event":"turn.ended","repo":"x","schema_version":1}`,
		"doctor check without status":    `{` + ok + `,"doctor_checks":[{"check":"x"}]}`,
		"doctor check with unknown key":  `{` + ok + `,"doctor_checks":[{"check":"x","status":"ok","why":"x"}]}`,
		"string score":                   `{` + ok + `,"scores":{"q0":"0.5"}}`,
		"number in a string array":       `{` + ok + `,"tools":[1]}`,
		"not an object":                  `[` + ok + `]`,
		"trailing data after the object": `{` + ok + `} {}`,
	}
	for name, line := range rows {
		if _, err := Decode([]byte(line)); err == nil {
			t.Errorf("%s: decoded without error: %s", name, line)
		}
	}
	if _, err := Decode([]byte(`{` + ok + `}`)); err != nil {
		t.Errorf("the minimal valid line was refused: %v", err)
	}
}
