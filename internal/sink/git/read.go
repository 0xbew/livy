package git

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Source reads what git sinks pushed to a repository, for `livy drain`. It keeps its own full
// clone (never shallow, so the files added since an earlier tip can be listed) and never checks
// a tree out: the repository's files are read as blobs, so nothing in it is ever written to disk.
type Source struct{ s *Sink }

// NewSource checks the remote and folder as New does. The clone is made on the first Fetch.
func NewSource(c Config) (*Source, error) {
	s, err := New(c)
	if err != nil {
		return nil, err
	}
	return &Source{s: s}, nil
}

// URL names a file in the repository, for messages.
func (src *Source) URL(key string) string { return src.s.URL(key) }

// Fetch brings the branch's tip into the clone and returns its commit; "" when the branch does
// not exist yet (no orb has pushed).
func (src *Source) Fetch(ctx context.Context) (string, error) {
	s := src.s
	if err := s.clone(ctx); err != nil {
		return "", err
	}
	found, err := s.remoteHasBranch(ctx)
	if !found {
		return "", err
	}
	ref := "refs/remotes/origin/" + s.c.Branch
	if err := s.run(ctx, "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/"+s.c.Branch+":"+ref); err != nil {
		return "", err
	}
	out, err := s.gitOutput(ctx, s.c.Dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

// Added lists the files added or changed from commit from to commit to. With from "", or a from
// that is no longer an ancestor of to (the branch was rewritten), it lists every file at to.
func (src *Source) Added(ctx context.Context, from, to string) ([]string, error) {
	s := src.s
	args := []string{"ls-tree", "-r", "-z", "--name-only", to}
	if from != "" && s.run(ctx, "merge-base", "--is-ancestor", from, to) == nil {
		args = []string{"diff", "--name-only", "-z", "--no-renames", "--diff-filter=AM", from, to}
	}
	out, err := s.gitOutput(ctx, s.c.Dir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range bytes.SplitSeq(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// Read is the file's bytes at commit. A file larger than limit is refused unread: whoever
// controls a pushing machine controls what is in the repository.
func (src *Source) Read(ctx context.Context, commit, path string, limit int64) ([]byte, error) {
	s := src.s
	spec := commit + ":" + path
	out, err := s.gitOutput(ctx, s.c.Dir, "cat-file", "-s", spec)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("git cat-file -s %s: %w", path, err)
	}
	if size > limit {
		return nil, fmt.Errorf("%s is %d bytes, over the %d a segment may be", path, size, limit)
	}
	return s.gitOutput(ctx, s.c.Dir, "cat-file", "blob", spec)
}
