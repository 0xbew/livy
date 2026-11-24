package profile

import (
	"slices"
	"strings"

	"github.com/0xbew/livy/internal/why"
)

// commitFiles is the paths each commit changes, read from git in two runs per batch of new
// commits and kept for the whole report. A session's earlier commits are the same for each of
// its commits, and the ledger names commits of every repo the session touched, so one git run
// per commit cost minutes on a long session. The paths are compared, never printed.
type commitFiles struct {
	changed map[string]map[string]bool // by the sha as asked for; nil for a commit this clone lacks
	asked   map[string]bool
}

// know reads every sha in shas not read before: which are commits in this clone (one
// `git cat-file --batch-check`), then the paths each of those changes (one `git diff-tree --stdin`).
func (cf *commitFiles) know(shas []string) error {
	if cf.asked == nil {
		cf.asked, cf.changed = map[string]bool{}, map[string]map[string]bool{}
	}
	var fresh []string
	for _, s := range shas {
		if s != "" && !cf.asked[s] && !slices.Contains(fresh, s) {
			fresh = append(fresh, s)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	full, err := commitIDs(fresh)
	if err != nil {
		return err
	}
	spellings := map[string][]string{} // full id -> each sha asked for that names it (a short and a full sha can)
	var ids []string
	for i, s := range fresh {
		if full[i] == "" {
			continue
		}
		if spellings[full[i]] == nil {
			ids = append(ids, full[i])
		}
		spellings[full[i]] = append(spellings[full[i]], s)
	}
	if len(ids) > 0 {
		if err := cf.diffTree(ids, spellings); err != nil {
			return err
		}
	}
	for _, s := range fresh {
		cf.asked[s] = true
	}
	return nil
}

// diffTree reads the paths each of ids (full commit ids) changes, in one `git diff-tree --stdin`.
func (cf *commitFiles) diffTree(ids []string, spellings map[string][]string) error {
	out, err := why.GitInput(strings.Join(ids, "\n")+"\n", "diff-tree", "--stdin", "-r", "--root", "--no-renames", "--name-only")
	if err != nil {
		return err
	}
	byID := make(map[string]map[string]bool, len(ids))
	for _, id := range ids {
		byID[id] = map[string]bool{} // a commit that changes nothing prints no header
	}
	var cur map[string]bool
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if files, ok := byID[line]; ok {
			cur = files
			continue
		}
		if line != "" && cur != nil {
			cur[line] = true
		}
	}
	for id, files := range byID {
		for _, s := range spellings[id] {
			cf.changed[s] = files
		}
	}
	return nil
}

// files is the paths sha changes; false when this clone has no such commit or it was not read.
func (cf *commitFiles) files(sha string) (map[string]bool, bool) {
	f := cf.changed[sha]
	return f, f != nil
}

// commitIDs is each sha's full commit id in this clone, "" for one that is not a commit here.
func commitIDs(shas []string) ([]string, error) {
	out, err := why.GitInput(strings.Join(shas, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	ids := make([]string, len(shas))
	for i := range shas {
		if i < len(lines) {
			if id, kind, ok := strings.Cut(lines[i], " "); ok && kind == "commit" {
				ids[i] = id
			}
		}
	}
	return ids, nil
}
