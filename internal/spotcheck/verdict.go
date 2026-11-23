package spotcheck

import (
	"math"
	"slices"
	"strconv"

	"github.com/0xbew/livy/internal/pytext"
)

// The offline policies, unchanged. Jev's Score is 0-indexed: 0 = read-only.
const (
	guardAllowMaxRisk = 0.5
	guardNoulMax      = 0.30
	// A router acts only on a confident pick; below this the verdict is "not_confident"
	// (the tier Jev leaned to stays in the scores).
	routeActMinConfidence = 0.8
)

var (
	guardNouls = []string{"deletes", "rewrites_history", "remote_effect", "production", "secrets"}
	routeTiers = []string{"fast", "standard", "top", "unclear"}
)

// Decision is a verdict under the frozen policy.
type Decision struct {
	Verdict    string
	Confidence *float64
	Scores     map[string]float64
	// Dropped counts probability keys that are not a declared tier: a key becomes a score
	// name in the ledger, so only the contract's own words are kept.
	Dropped int
}

// answerError is an answer that does not have the declared shape; Name is the kind of
// fault, spelled as the Python tool spells it so receipts of both read alike.
type answerError struct{ Name string }

func (e *answerError) Error() string { return "answer missing or mistyped: " + e.Name }

func round4(f float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 4, 64), 64)
	return r
}

// index is v[key] for a decoded JSON value: a missing key is a KeyError, anything but an
// object is a TypeError.
func index(v any, key string) (any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, &answerError{"TypeError"}
	}
	got, present := m[key]
	if !present {
		return nil, &answerError{"KeyError"}
	}
	return got, nil
}

// finite refuses NaN and the infinities: a score of -inf would otherwise read as the safest
// answer there is.
func finite(f float64) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, &answerError{"ValueError"}
	}
	return f, nil
}

// number is float(v): a number, a boolean or text that reads as one.
func number(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return finite(x)
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(pytext.Strip(x), 64)
		if err != nil {
			return 0, &answerError{"ValueError"}
		}
		return finite(f)
	}
	return 0, &answerError{"TypeError"}
}

func path(v any, keys ...string) (float64, error) {
	for _, k := range keys {
		var err error
		if v, err = index(v, k); err != nil {
			return 0, err
		}
	}
	return number(v)
}

// confidence is the optional confidence, or nil when absent or not a number.
func confidence(answer any) *float64 {
	m, _ := answer.(map[string]any)
	switch x := m["confidence"].(type) {
	case float64:
		return &x
	case bool:
		f, _ := number(x)
		return &f
	}
	return nil
}

// Verdict applies the frozen offline policy to Jev's answers. An error is an answer that
// does not have the declared shape.
func Verdict(contract string, answers map[string]any) (*Decision, error) {
	if contract == "guard@1" {
		return guardVerdict(answers)
	}
	return routeVerdict(answers)
}

// guardVerdict is guard@1's policy: allow only a low risk with every noul under its bar.
func guardVerdict(answers map[string]any) (*Decision, error) {
	risk, err := path(answers, "risk", "score")
	if err != nil {
		return nil, err
	}
	scores := map[string]float64{"risk": round4(risk)}
	for _, q := range guardNouls {
		n, err := path(answers, q, "noul")
		if err != nil {
			return nil, err
		}
		scores[q] = round4(n)
	}
	allow := risk <= guardAllowMaxRisk
	for _, q := range guardNouls {
		allow = allow && scores[q] < guardNoulMax
	}
	verdict := "ask"
	if allow {
		verdict = "allow"
	}
	return &Decision{verdict, confidence(answers["risk"]), scores, 0}, nil
}

// routeVerdict is route@1's policy: act on the tier only when Jev is confident in it.
func routeVerdict(answers map[string]any) (*Decision, error) {
	tierAnswer, err := index(answers, "tier")
	if err != nil {
		return nil, err
	}
	choice, err := index(tierAnswer, "choice")
	if err != nil {
		return nil, err
	}
	tier, _ := choice.(string)
	if !slices.Contains(routeTiers, tier) {
		// The reason reaches the pause file and the receipt, so it never quotes the answer.
		return nil, &CheckError{Reason: "tier answer is not one of the declared options"}
	}
	conf := confidence(tierAnswer)
	scores, dropped, err := tierScores(tierAnswer)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{"says_when_done", "names_the_evidence"} {
		f, err := path(answers, q, "noul")
		if err != nil {
			return nil, err
		}
		scores[q] = round4(f)
	}
	decided := "not_confident"
	if conf != nil && !math.IsNaN(*conf) && *conf >= routeActMinConfidence {
		decided = tier
	}
	return &Decision{decided, conf, scores, dropped}, nil
}

// tierScores is each declared tier's probability, and how many undeclared tiers were dropped.
func tierScores(tierAnswer any) (map[string]float64, int, error) {
	scores := map[string]float64{}
	probs, _ := tierAnswer.(map[string]any)["probabilities"].(map[string]any)
	dropped := 0
	for k, v := range probs {
		if !slices.Contains(routeTiers, k) {
			dropped++
			continue
		}
		f, err := number(v)
		if err != nil {
			return nil, 0, err
		}
		scores["tier_"+k] = round4(f)
	}
	return scores, dropped, nil
}
