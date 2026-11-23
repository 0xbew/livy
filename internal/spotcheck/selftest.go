package spotcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/0xbew/scrub"
)

// Selftest proves the checks offline: redaction, the frozen policies, the own-words guard,
// the receipt's hash and the pre-send audit, which must stop a body holding an address
// before any connection is tried. keyFound says whether checks are on. It returns the exit
// code.
func Selftest(stdout, stderr io.Writer, keyFound func() bool) int {
	var failures []string
	for _, check := range []func() []string{redactionFailures, policyFailures, ownWordsFailures, receiptFailures,
		auditFailures} {
		failures = append(failures, check()...)
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(stderr, "selftest FAILED: %s\n", f)
		}
		return 1
	}
	key := "NOT found (checks are off)"
	if keyFound() {
		key = "found"
	}
	fmt.Fprintf(stdout, "selftest ok: redaction, frozen policies, own-words guard, receipt, pre-send audit; key %s\n", key)
	return 0
}

// redactionFailures checks secrets leave a command and PATH stays.
func redactionFailures() []string {
	var failures []string
	sample := "PGPASSWORD=hunter2 psql postgres://app:s3cret@db/x -H 'Authorization: Bearer abc.def' " +
		"&& export api_key=lower1 && echo PATH=$PATH"
	red := scrub.Redact(sample, commandChars)
	for _, leaked := range []string{"hunter2", "s3cret", "abc.def", "lower1"} {
		if strings.Contains(red, leaked) {
			failures = append(failures, fmt.Sprintf("redaction left %q", leaked))
		}
	}
	if !strings.Contains(red, "PATH=$PATH") {
		failures = append(failures, "redaction removed PATH, which is not a secret")
	}
	return failures
}

// policyFailures checks the frozen guard and route policies on known answers.
func policyFailures() []string {
	var failures []string
	noul := func(v float64, hot string) map[string]any {
		m := map[string]any{"risk": map[string]any{"score": 0.2}}
		for _, q := range guardNouls {
			x := 0.0
			if q == hot {
				x = v
			}
			m[q] = map[string]any{"noul": x}
		}
		return m
	}
	if d, err := Verdict("guard@1", noul(0.9, "remote_effect")); err != nil || d.Verdict != "ask" {
		failures = append(failures, "a remote effect was allowed")
	}
	if d, err := Verdict("guard@1", noul(0.05, "")); err != nil || d.Verdict != "allow" {
		failures = append(failures, "a read-only command was not allowed")
	}
	unsure := map[string]any{
		"tier":               map[string]any{"choice": "fast", "confidence": 0.5, "probabilities": map[string]any{"fast": 0.5}},
		"says_when_done":     map[string]any{"noul": 0.9},
		"names_the_evidence": map[string]any{"noul": 0.9}}
	if d, err := Verdict("route@1", unsure); err != nil || d.Verdict != "not_confident" {
		failures = append(failures, "an unconfident tier was acted on")
	}
	return failures
}

// ownWordsFailures checks a brief quoting the engineer's own prompt is skipped.
func ownWordsFailures() []string {
	var failures []string
	dir, err := os.MkdirTemp("", "livy-spotcheck-")
	if err != nil {
		return []string{fmt.Sprintf("no temporary folder: %v", err)}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	transcript := filepath.Join(dir, "t.jsonl")
	prompt := "please look into why the nightly slate job keeps stranding contests tonight"
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	if os.WriteFile(transcript, append(line, '\n'), 0o600) != nil {
		failures = append(failures, "no transcript written")
	}
	quoting := map[string]any{"tool_name": "Agent", "transcript_path": transcript, "tool_input": map[string]any{
		"prompt": "The user said: " + prompt + ". Find the cause."}}
	if p := Plan(quoting, scrub.NoDenylist, "r"); p == nil || p.Skipped == "" {
		failures = append(failures, "a brief quoting the engineer's words was not skipped")
	}
	return failures
}

// receiptFailures checks the receipt's hash is a 16-hex digest that tells states apart.
func receiptFailures() []string {
	state := obj{{"command", "ls"}}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(InputSHA(state)) || InputSHA(state) == InputSHA(obj{{"command", "pwd"}}) {
		return []string{"the receipt's hash is not a 16-hex digest of the state"}
	}
	return nil
}

// auditFailures checks a broken audit fails here, and never sends the planted address.
func auditFailures() []string {
	a := &Asker{Endpoint: "http://127.0.0.1:1/", Client: nil, Budget: callBudget, Timeout: requestTimeout}
	var refused *AuditRefusedError
	if _, _, err := a.Ask("selftest", "guard@1", obj{{"command", "mail someone@example.test"}}, scrub.NoDenylist); !errors.As(err, &refused) {
		return []string{"a body holding an address got past the pre-send audit"}
	}
	return nil
}
