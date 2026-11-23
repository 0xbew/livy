package incident

import (
	"reflect"
	"testing"

	"github.com/0xbew/livy/internal/why"
)

// Guards: an incident pack that counts a session or prompt once per commit it spans (one real
// session spanned 190 PRs), or that leaves out the commits no agent session was found for,
// which are the ones the review must ask about by hand.
func TestTotalsCountEachSessionAndPromptOnce(t *testing.T) {
	reports := []why.Report{
		{SHA: "aaaaaaaaaaaaaaaa", Sessions: []why.Session{
			{ID: "s-1", Prompts: []string{"p-1", "p-2"}, Ticket: "PROJ-7"},
			{ID: "s-2", Prompts: []string{"q-1"}, Ticket: "no-ticket"},
		}},
		{SHA: "bbbbbbbbbbbbbbbb"},
		{SHA: "cccccccccccccccc", Sessions: []why.Session{
			{ID: "s-1", Prompts: []string{"p-2", "p-3"}, Ticket: "PROJ-7"},
		}},
	}
	want := Totals{Commits: 3, AgentCommits: 2, Sessions: []string{"s-1", "s-2"}, Prompts: 4,
		Tickets: []string{"PROJ-7", "no-ticket"}, NoAgent: []string{"bbbbbbbbbbbb"}}
	if got := total(reports); !reflect.DeepEqual(got, want) {
		t.Errorf("total =\n%+v\nwant\n%+v", got, want)
	}
}
