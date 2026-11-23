package why

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Guards: on a main whose pull requests are squash-merged, every commit is new, so why found no
// agent session behind any of them and the on-call looked up each PR's commits by hand. The
// fixtures are GitHub's answers for a real PR, #431 (squash 7f8facc, three commits), cut to the
// fields read, with neutral commit subjects; the ledger lines are placed between those commits' times.
func TestSquashMergeFindsThePRsCommits(t *testing.T) {
	const (
		squash  = "7f8faccacfdea4ad1d5bbcd9d3f5c3eed534179f"
		subject = "match: narrow a same-file match by its quoted title (#431)"
		session = "4e7270ba-f9f4-4470-ba5d-9f1d26d94dd5"
	)
	events := []Event{
		{Event: "prompt.submitted", TS: "2026-11-06T02:00:00Z", SessionID: session, PromptID: "p-1"},
		{Event: "prompt.submitted", TS: "2026-11-06T02:30:00Z", SessionID: session, PromptID: "p-2"},
		{Event: "prompt.submitted", TS: "2026-11-06T03:40:00Z", SessionID: session, PromptID: "p-3"},
	}
	fixture := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "squash", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pulls, commits := fixture("pulls-of-7f8facc.json"), fixture("pr-431-commits.json")
	type original struct{ SHA, Basis, Prompts string }
	found := []original{
		{"e8162ded11f6", basisNoEarlier, "p-1"},
		{"c977ff3d267b", basisTrailer + "e8162ded11f6", "p-2"},
		{"5922a0db74fa", basisTrailer + "c977ff3d267b", "p-3"},
	}
	notSignedIn := &ghError{msg: "gh: To get started with GitHub CLI, please run:  gh auth login"}
	for _, tc := range []struct {
		name        string
		answers     map[string]any // endpoint -> []byte or error
		wantPR      int
		want        []original
		wantCalls   int // gh calls over two lookups of the same commit
		wantProblem bool
	}{
		{name: "the API names the PR", wantPR: 431, want: found, wantCalls: 4, answers: map[string]any{
			"repos/acme/app/commits/" + squash + "/pulls": pulls,
			"repos/acme/app/pulls/431/commits":            commits}},
		{name: "the API does not know the commit; its subject names the PR", wantPR: 431, want: found, wantCalls: 4,
			answers: map[string]any{
				"repos/acme/app/commits/" + squash + "/pulls": &ghError{msg: "gh: No commit found for SHA (HTTP 422)", answered: true},
				"repos/acme/app/pulls/431/commits":            commits}},
		{name: "gh is not signed in: said once, never asked again", wantPR: 431, wantCalls: 1, wantProblem: true,
			answers: map[string]any{
				"repos/acme/app/commits/" + squash + "/pulls": notSignedIn,
				"repos/acme/app/pulls/431/commits":            notSignedIn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				for _, a := range args {
					endpoint, _, _ := strings.Cut(a, "?")
					switch v := tc.answers[endpoint].(type) {
					case []byte:
						return v, nil
					case error:
						return nil, v
					}
				}
				t.Fatalf("gh %v: no answer arranged", args)
				return nil, nil
			}
			l := &prLookup{gh: gh, repo: "acme/app"}
			c := Commit{SHA: squash, Committed: at("2026-11-13T00:00:00Z")}
			var got *Squash
			for range 2 {
				got = l.find(t.Context(), c, subject, events)
			}
			var originals []original
			for _, o := range got.Originals {
				for _, s := range o.Report.Sessions {
					originals = append(originals, original{short(o.Commit.SHA), s.Basis, strings.Join(s.Prompts, ",")})
				}
			}
			if got.PR != tc.wantPR || !reflect.DeepEqual(originals, tc.want) || calls != tc.wantCalls || (got.Problem != "") != tc.wantProblem {
				t.Errorf("find(%s) = PR %d, originals %+v, problem %q after %d gh calls;\nwant PR %d, originals %+v, a problem %v after %d",
					short(squash), got.PR, originals, got.Problem, calls, tc.wantPR, tc.want, tc.wantProblem, tc.wantCalls)
			}
		})
	}
}
