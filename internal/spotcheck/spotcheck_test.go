package spotcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xbew/scrub"
)

// Expected values below were computed by running the Python recorder's spotcheck.py on the
// same input (python3.13, the Python reference's main), not derived from this code.

func TestFrozenContractsMatchPython(t *testing.T) {
	// Guards: a question's text or order changing without a new contract id, which would make
	// live receipts incomparable with the offline result they were frozen against.
	want := map[string]string{
		"guard@1": "e1175b1b612192dde1d6c9f3665e79056a9bdf034dcfaeb69b05b3d4c696af70",
		"route@1": "723e3cc460733b185a633fb7b6c094d6f6ecdc0866c4f636358d757e1c1affd0",
	}
	for id, digest := range want {
		sum := sha256.Sum256([]byte(dumps(contracts[id])))
		if got := hex.EncodeToString(sum[:]); got != digest {
			t.Errorf("%s digest %s, want %s", id, got, digest)
		}
	}
}

func TestInputSHAMatchesPython(t *testing.T) {
	// Guards: a receipt's input_sha differing from the Python recorder's for the same text, so
	// the two recorders' receipts for one command could not be joined.
	cases := []struct {
		state obj
		want  string
	}{
		{obj{{"repo", "o/r"}, {"command", "ls -la"}, {"agent_description", ""}}, "9a33fc29115b1899"},
		{obj{{"repo", "o/r"}, {"command", "echo \"héllo\" \\ \n\t 😀 <&> \x7f"}, {"agent_description", "de sc"}}, "340664b850da1d9d"},
		{obj{{"subagent_type", "general-purpose"}, {"description", "d"}, {"brief", "Find x. é中"}}, "63dec69f0ba6c065"},
	}
	for _, c := range cases {
		if got := InputSHA(c.state); got != c.want {
			t.Errorf("InputSHA = %s, want %s", got, c.want)
		}
	}
}

func TestOpaqueScriptMatchesPython(t *testing.T) {
	// Guards: a script run by name going unflagged (its effect is invisible to Jev), or a
	// `bash -c` one-liner (whose text Jev reads) being flagged.
	for command, want := range map[string]bool{
		"bash infra/bucket.sh --apply": true, "bash -c 'echo hi'": false, "bash -cx foo": true, "sh  -c x": false,
		"./run.sh": true, "echo ./run.sh": true, "make test": true, "makefile x": false, "just build": true,
		"npm run build": true, "npm install": false, "a;bash x.sh": true, "x|zsh   y": true, "bash": false,
		"(./go)": true, "bash -c ./inner.sh": true, "yarn  run": true, "pnpm runx": false, "bash -c;./x": true,
		"ls && make": true, "docker run x": false, "MAKE x": false,
	} {
		if got := OpaqueScript(command); got != want {
			t.Errorf("OpaqueScript(%q) = %v, want %v", command, got, want)
		}
	}
}

func guardAnswers(risk float64, hot string, noul float64) map[string]any {
	m := map[string]any{"risk": map[string]any{"score": risk, "confidence": 0.9}}
	for _, q := range guardNouls {
		v := 0.0
		if q == hot {
			v = noul
		}
		m[q] = map[string]any{"noul": v}
	}
	return m
}

func routeAnswers(conf float64, tier string) map[string]any {
	return map[string]any{
		"tier": map[string]any{"choice": tier, "confidence": conf,
			"probabilities": map[string]any{"fast": 0.05, "standard": 0.86}},
		"says_when_done": map[string]any{"noul": 0.9}, "names_the_evidence": map[string]any{"noul": 0.7}}
}

func routeWithProbability(key string, p float64) map[string]any {
	a := routeAnswers(0.9, "standard")
	a["tier"].(map[string]any)["probabilities"].(map[string]any)[key] = p
	return a
}

func TestVerdictMatchesPython(t *testing.T) {
	// Guards: the frozen policy drifting at its edges: a yes/no probability is compared after
	// rounding to four places, a risk at exactly 0.5 is allowed, and a router acts at 0.8 and
	// not at 0.79999.
	cases := []struct {
		name, contract string
		answers        map[string]any
		verdict        string
		score          string
		want           float64
		absent         bool // the score must not be recorded at all
	}{
		{"risk at the bar", "guard@1", guardAnswers(0.5, "", 0), "allow", "risk", 0.5, false},
		{"risk past the bar", "guard@1", guardAnswers(0.5000001, "", 0), "ask", "risk", 0.5, false},
		{"noul rounds up to the bar", "guard@1", guardAnswers(0, "remote_effect", 0.29996), "ask", "remote_effect", 0.3, false},
		{"noul rounds down under the bar", "guard@1", guardAnswers(0, "remote_effect", 0.29994), "allow", "remote_effect", 0.2999, false},
		{"confident at 0.8", "route@1", routeAnswers(0.8, "standard"), "standard", "tier_standard", 0.86, false},
		{"unconfident under 0.8", "route@1", routeAnswers(0.79999, "standard"), "not_confident", "says_when_done", 0.9, false},
		{"top", "route@1", routeAnswers(0.95, "top"), "top", "names_the_evidence", 0.7, false},
		{"undeclared probability key is not a score name", "route@1", routeWithProbability("rm -rf ~/secret", 0.5),
			"standard", "tier_rm -rf ~/secret", 0, true},
		{"declared probability key kept", "route@1", routeWithProbability("rm -rf ~/secret", 0.5),
			"standard", "tier_fast", 0.05, false},
	}
	for _, c := range cases {
		d, err := Verdict(c.contract, c.answers)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got, recorded := d.Scores[c.score]
		if d.Verdict != c.verdict || got != c.want || recorded == c.absent {
			t.Errorf("%s: verdict %s, %s %v (recorded %v); want %s, %v (absent %v)", c.name, d.Verdict, c.score, got,
				recorded, c.verdict, c.want, c.absent)
		}
	}
	// An error's text goes into the pause file and the receipt, so it never carries the answer.
	for _, c := range []struct {
		contract string
		answers  map[string]any
		want     string
	}{
		{"guard@1", map[string]any{}, "answer missing or mistyped: KeyError"},
		{"guard@1", map[string]any{"risk": map[string]any{"score": "x"}}, "answer missing or mistyped: ValueError"},
		{"guard@1", map[string]any{"risk": nil}, "answer missing or mistyped: TypeError"},
		{"route@1", routeAnswers(0.9, "ask jordan@example.test"), "tier answer is not one of the declared options"},
		{"route@1", routeAnswers(0.9, ""), "tier answer is not one of the declared options"},
	} {
		_, err := Verdict(c.contract, c.answers)
		if err == nil || err.Error() != c.want {
			t.Errorf("Verdict(%s, %v) error = %v, want %s", c.contract, c.answers, err, c.want)
		}
	}
}

func TestOwnWordsGuardMatchesPython(t *testing.T) {
	// Guards: a brief that quotes the engineer being sent to a third party: a shared run of
	// eight words or a whole short prompt must be caught, a one-word prompt must not.
	prompts := [][]string{
		words("please look into why the nightly slate job keeps stranding contests tonight"),
		words("go ahead now"), words("ok")}
	for text, want := range map[string]bool{
		"The user said: please look into why the nightly slate job keeps stranding contests tonight. Fix.": true,
		"then go ahead now thanks": true, "ok": false,
		"look into why the nightly slate job keeps stranding": true,
		"please look into why the nightly slate job keeps":    true,
	} {
		if got := RepeatsOwner(text, prompts); got != want {
			t.Errorf("RepeatsOwner(%q) = %v, want %v", text, got, want)
		}
	}
}

func asker(srv *httptest.Server) *Asker {
	return &Asker{Endpoint: srv.URL, Client: srv.Client(), Budget: 5 * time.Second, Timeout: time.Second}
}

func TestAuditRefusalSendsNothing(t *testing.T) {
	// Guards: text the audit refuses reaching the network. The scrubber is not trusted to be
	// the last line: the exact body is audited and a hit is a receipt, with no request made.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	planned := &Planned{Check: "guard", Contract: "guard@1", State: obj{{"command", "mail someone@example.test"}}}
	got := asker(srv).Decide(planned, "k", Denylist{D: scrub.NoDenylist}, t.TempDir(), time.Now())
	if got["verdict"] != "skipped" || !strings.Contains(got["reason"].(string), "the pre-send audit refused it (") ||
		!strings.HasSuffix(got["reason"].(string), "); nothing sent") {
		t.Errorf("receipt = %v", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d request(s) reached the server, want 0", n)
	}
}

func TestAuthFailurePausesChecks(t *testing.T) {
	// Guards: a rejected key costing one failed call per command in every session: the first
	// failure is one error receipt carrying the pause, and the next call makes no request.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir, now := t.TempDir(), time.Unix(1793000000, 0)
	planned := &Planned{Check: "guard", Contract: "guard@1", State: obj{{"command", "ls"}}}
	a := asker(srv)
	first := a.Decide(planned, "k", Denylist{D: scrub.NoDenylist}, dir, now)
	if first["verdict"] != "error" || first["paused_seconds"] != int64(3600) ||
		first["reason"] != "HTTP 401 (key rejected); checks paused 60 min on this machine" {
		t.Errorf("first receipt = %v", first)
	}
	if second := a.Decide(planned, "k", Denylist{D: scrub.NoDenylist}, dir, now.Add(time.Minute)); second != nil {
		t.Errorf("second call recorded %v while paused", second)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestReceiptKeepsNoResponseText(t *testing.T) {
	// Guards: free text in Jev's reply reaching the ledger: the answering model is recorded
	// only when it reads as a model id, and an undeclared probability key is dropped and
	// counted rather than becoming a score name.
	cases := []struct {
		name     string
		model    any
		extraKey string
		jev      string
		mismatch bool
		dropped  any
	}{
		{"pinned model", "jev-1.13.0", "", "jev-1.13.0", false, nil},
		{"another version is kept and flagged", "jev-1.12.0", "", "jev-1.12.0", true, nil},
		{"no model reads as the pinned one, flagged", nil, "", "jev-1.13.0", true, nil},
		{"text after a model id", "jev-1.13.0 ask jordan pellwright", "", "unexpected-model", true, nil},
		{"not a model id at all", "Jordan Pellwright", "", "unexpected-model", true, nil},
		{"not a string", 7.0, "", "unexpected-model", true, nil},
		{"undeclared probability key", "jev-1.13.0", "mail jordan", "jev-1.13.0", false, 1},
	}
	for _, c := range cases {
		answers := routeAnswers(0.9, "standard")
		if c.extraKey != "" {
			answers = routeWithProbability(c.extraKey, 0.5)
		}
		body := map[string]any{"answers": answers}
		if c.model != nil {
			body["model"] = c.model
		}
		raw, _ := json.Marshal(body)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw) }))
		planned := &Planned{Check: "route", Contract: "route@1", State: obj{{"brief", "Find x."}}}
		got := asker(srv).Decide(planned, "k", Denylist{D: scrub.NoDenylist}, t.TempDir(), time.Now())
		srv.Close()
		mismatch, _ := got["jev_model_mismatch"].(bool)
		if got["jev_model"] != c.jev || mismatch != c.mismatch || got["scores_dropped"] != c.dropped {
			t.Errorf("%s: jev_model %v, mismatch %v, scores_dropped %v; want %s, %v, %v", c.name, got["jev_model"],
				mismatch, got["scores_dropped"], c.jev, c.mismatch, c.dropped)
		}
		for k := range got["scores"].(map[string]float64) {
			if strings.Contains(k, "jordan") {
				t.Errorf("%s: score name %q carries response text", c.name, k)
			}
		}
	}
}
