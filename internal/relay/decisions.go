package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xbew/livy/internal/ledger"
)

// Claude Code's and Codex's tool decisions (OTEL's tool_decision) are the only record of a tool
// call approved, not just one refused. The otel stream ships and is deleted, so the receiver also
// keeps each decision as one text-free, ledger-shaped line under DecisionsDir, one file per UTC
// day, for `livy why`. They are never shipped; Prune deletes days older than DecisionKeepDays.

// DecisionKeepDays is how long kept decisions stay, as long as the prompt vault keeps text.
const DecisionKeepDays = 90

const decisionDay = "2006-01-02"

// decisionEvents are the log events that carry a tool decision, by client.
var decisionEvents = map[string]string{"tool_decision": "claude-code", "codex.tool_decision": "codex"}

// otlpLogs is the part of an OTLP/JSON logs export a decision is read from.
type otlpLogs struct {
	ResourceLogs []struct {
		ScopeLogs []struct {
			LogRecords []struct {
				Attributes []otlpAttr `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otlpAttr struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
	} `json:"value"`
}

// decision is one kept line; its field names are the ledger's, so `livy why` reads it as one,
// and its fields are in the ledger's sorted-key order.
type decision struct {
	Decision      string `json:"decision"`
	Source        string `json:"decision_source"`
	Event         string `json:"event"`
	Harness       string `json:"harness"`
	PromptID      string `json:"prompt_id,omitempty"`
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Tool          string `json:"tool"`
	ToolUseID     string `json:"tool_use_id,omitempty"`
	TS            string `json:"ts"`
}

// decisionsIn is every tool decision in a redacted logs export, in the order sent.
func decisionsIn(doc map[string]any) ([]decision, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("decisions: %w", err)
	}
	var logs otlpLogs
	if err := json.Unmarshal(raw, &logs); err != nil {
		return nil, fmt.Errorf("decisions: %w", err)
	}
	var out []decision
	for _, rl := range logs.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				if d, ok := decisionOf(lr.Attributes); ok {
					out = append(out, d)
				}
			}
		}
	}
	return out, nil
}

// decisionOf is the decision a log record carries, false for any other record or one missing a
// session, a decision or a time.
func decisionOf(attrs []otlpAttr) (decision, bool) {
	a := map[string]string{}
	for _, kv := range attrs {
		if kv.Value.String != nil {
			a[kv.Key] = *kv.Value.String
		}
	}
	harness, ok := decisionEvents[a["event.name"]]
	if !ok {
		return decision{}, false
	}
	session := a["session.id"]
	if harness == "codex" {
		session = a["conversation.id"]
	}
	at, err := time.Parse(time.RFC3339Nano, a["event.timestamp"])
	if session == "" || a["decision"] == "" || err != nil {
		return decision{}, false
	}
	return decision{SchemaVersion: 1, TS: at.UTC().Format(time.RFC3339Nano), Event: "tool.decision", Harness: harness,
		SessionID: session, PromptID: a["prompt.id"], ToolUseID: firstOf(a["tool_use_id"], a["call_id"]),
		Tool: a["tool_name"], Decision: a["decision"], Source: a["source"]}, true
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// keepDecisions appends ds under dataDir's DecisionsDir, in the file of the UTC day they were
// received (a client's clock decides nothing about retention), owner-only. The caller holds
// the Store's lock: exports arrive concurrently.
// unchecked is why the day file's last line could not be checked for a torn write: the
// decisions were then kept without it.
func keepDecisions(dataDir string, ds []decision, now time.Time) (unchecked, err error) {
	var lines []byte
	for _, d := range ds {
		line, err := json.Marshal(d)
		if err != nil {
			return nil, fmt.Errorf("decisions: %w", err)
		}
		lines = append(append(lines, line...), '\n')
	}
	dir := filepath.Join(dataDir, DecisionsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("decisions: %w", err)
	}
	return appendFile(filepath.Join(dir, now.UTC().Format(decisionDay)+".jsonl"), lines)
}

func appendFile(path string, b []byte) (unchecked, err error) {
	t, err := ledger.OpenTail(path)
	if err != nil {
		return nil, fmt.Errorf("decisions: %w", err)
	}
	defer func() {
		if cerr := t.File.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("decisions: %w", cerr)
		}
	}()
	if _, err := t.File.Write(t.Lead(b)); err != nil {
		return nil, fmt.Errorf("decisions: %w", err)
	}
	return t.Unchecked, nil
}

// PruneDecisions deletes kept decision days older than DecisionKeepDays.
func PruneDecisions(dataDir string, now time.Time) error {
	dir := filepath.Join(dataDir, DecisionsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decisions: %w", err)
	}
	var errs []error
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".jsonl")
		day, perr := time.Parse(decisionDay, name)
		if !ok || perr != nil || now.Sub(day.Add(24*time.Hour)) < DecisionKeepDays*24*time.Hour {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
