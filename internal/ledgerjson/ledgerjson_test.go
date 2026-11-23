package ledgerjson

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// Every expected line is Python 3.9's own output (testdata/gen.py), so the expected value
// can disagree with this encoder.
func TestMarshalMatchesPythonLedgerBytes(t *testing.T) {
	// Guards: a re-encoded ledger line whose bytes differ from Python's, which changes its segment's key.
	f, err := os.Open("testdata/python_dumps.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		var row struct {
			Kind string
			In   json.RawMessage
			Hex  string
			Out  string
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		var in any
		switch row.Kind {
		case "value":
			dec := json.NewDecoder(bytes.NewReader(row.In))
			dec.UseNumber()
			if err := dec.Decode(&in); err != nil {
				t.Fatal(err)
			}
		case "float":
			f, err := strconv.ParseFloat(row.Hex, 64)
			if err != nil {
				t.Fatalf("line %d: %v", n, err)
			}
			in = f
		}
		got, err := Marshal(in)
		if err != nil {
			t.Errorf("line %d (%s): %v", n, row.Kind, err)
			continue
		}
		if string(got) != row.Out {
			t.Errorf("line %d (%s %s):\n got %s\nwant %s", n, row.Kind, row.Hex, got, row.Out)
		}
	}
	if n < 50 {
		t.Fatalf("read %d fixture lines, want the full set", n)
	}
}
