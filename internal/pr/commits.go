package pr

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/keyspace"
	"github.com/0xbew/livy/internal/why"
)

// recordedOnBranch is the commits recorded on o's branch that this clone has, oldest first: from
// the branch's index file, or from every line when there is no index or --scan.
func recordedOnBranch(o options) ([]string, error) {
	var events []why.Event
	x, err := keyspace.Open(dataDir())
	var unusable *keyspace.Unusable
	switch {
	case o.scan || errors.As(err, &unusable):
		events, _ = why.LoadLocal()
	case err != nil:
		return nil, err
	default:
		if events, err = branchEvents(x, o); err != nil {
			return nil, err
		}
		if x.Changed() {
			return nil, errors.New("the index was rebuilt while it was read; run livy pr again")
		}
	}
	shas := inClone(branchCommits(events, o.repo, o.branch))
	if len(shas) == 0 {
		return nil, fmt.Errorf("no commit recorded on %s %s is in this clone; pass --commits", o.repo, o.branch)
	}
	out, err := why.Git(append([]string{"rev-list", "--no-walk=sorted", "--reverse"}, shas...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// inClone is the shas this clone has commits for, in order.
func inClone(shas []string) []string {
	if len(shas) == 0 {
		return nil
	}
	out, err := why.GitInput(strings.Join(shas, "\n")+"\n", "cat-file", "--batch-check")
	if err != nil {
		return nil
	}
	var have []string
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[1] == "commit" && !slices.Contains(have, f[0]) {
			have = append(have, f[0])
		}
	}
	return have
}

// branchEvents is the branch file's lines, the pending ones, and the commit buckets of every
// commit recorded on the branch and of every sha those were rewritten into: a rewrite made on a
// detached HEAD (a rebase) is not in the branch's file.
func branchEvents(x *keyspace.Index, o options) ([]why.Event, error) {
	col := why.NewCollector()
	lines, err := x.Lines(keyspace.BranchFile(o.repo, o.branch))
	if err != nil {
		return nil, err
	}
	col.Lines(labelIndex, lines)
	pending, err := keyspace.PendingLedger(dataDir())
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		if err := col.File(labelPending, p); err != nil {
			return nil, err
		}
	}
	follow, read := map[string]bool{}, map[string]bool{}
	for grew := true; grew; {
		grew = false
		followed(col.Events(), o, follow)
		for sha := range follow {
			if b := keyspace.CommitBucket(sha); !read[b] {
				read[b], grew = true, true
				got, err := x.Lines(b)
				if err != nil {
					return nil, err
				}
				col.Lines(labelIndex, got)
			}
		}
	}
	return col.Events(), nil
}

// followed adds to follow every commit recorded on o's branch and every sha a followed one was
// rewritten into.
func followed(events []why.Event, o options, follow map[string]bool) {
	for _, e := range events {
		onBranch := e.Event == eventRecorded && e.Branch == o.branch && strings.EqualFold(e.Repo, o.repo)
		if (onBranch || e.Event == eventRewritten && follow[e.OldCommitSHA]) && e.CommitSHA != "" {
			follow[e.CommitSHA] = true
		}
	}
}
