package spotcheck

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// modelID is the shape of a Jev model id. The reply's model string reaches the ledger, so
// anything else is recorded as unexpectedModel, never as the text it carried.
var modelID = regexp.MustCompile(`^jev-[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}$`)

const unexpectedModel = "unexpected-model"

// answeringModel is the model to record for a reply and whether it differs from the pinned
// one; a reply that names none is recorded as the pinned model and still flagged.
func answeringModel(v any) (string, bool) {
	switch s, isStr := v.(string); {
	case v == nil || (isStr && s == ""):
		return Model, true
	case isStr && modelID.MatchString(s):
		return s, s != Model
	}
	return unexpectedModel, true
}

// Receipt is the decision part of a check.decision line: its verdict and what explains it.
type Receipt map[string]any

// Decide makes the check for one planned tool call and returns what to add to the receipt,
// or nil when nothing is recorded (checks are paused: the pause was recorded once, when it
// began). It never returns an error: a check that could not be made is a receipt with
// verdict "error" and the reason, not a ledger.error, which doctor reads as "capture is
// broken".
func (a *Asker) Decide(p *Planned, key string, dl Denylist, dir string, now time.Time) Receipt {
	skipped := func(why string) Receipt { return Receipt{"verdict": "skipped", "reason": why} }
	if dl.Problem != "" {
		return skipped("denylist unusable, so nothing is sent: " + dl.Problem)
	}
	if p.Skipped != "" {
		return skipped(p.Skipped)
	}
	if Paused(dir, now) {
		return nil
	}
	resp, ms, err := a.Ask(key, p.Contract, p.State, dl.D)
	var d *Decision
	if err == nil {
		answers, _ := resp["answers"].(map[string]any)
		d, err = Verdict(p.Contract, answers)
	}
	var refused *AuditRefusedError
	var failed *CheckError
	var shape *answerError
	switch {
	case err == nil:
	case errors.As(err, &refused):
		return skipped(fmt.Sprintf("the pre-send audit refused it (%s); nothing sent", refused))
	default:
		if errors.As(err, &shape) {
			failed = &CheckError{Reason: shape.Error()}
		} else if !errors.As(err, &failed) {
			failed = &CheckError{Reason: err.Error()}
		}
		pause := pauseAfterError
		if failed.Auth {
			pause = pauseAfterAuth
		}
		out := Receipt{"verdict": "error"}
		if Pause(dir, pause, failed.Reason, now) {
			out["reason"] = fmt.Sprintf("%s; checks paused %d min on this machine", failed.Reason, int(pause.Minutes()))
			out["paused_seconds"] = int64(pause.Seconds())
		} else {
			out["reason"] = failed.Reason + "; the pause could not be recorded, so the next call retries"
		}
		return out
	}
	answeredBy, mismatch := answeringModel(resp["model"])
	out := Receipt{"verdict": d.Verdict, "scores": d.Scores, "latency_ms": ms, "jev_model": answeredBy}
	if mismatch {
		out["jev_model_mismatch"] = true
	}
	if d.Dropped > 0 {
		out["scores_dropped"] = d.Dropped
	}
	if d.Confidence != nil {
		out["confidence"] = round4(*d.Confidence)
	}
	return out
}
