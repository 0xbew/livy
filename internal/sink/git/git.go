// Package git is the repository sink: each object becomes a file at <key> in a Git repository
// this machine can push to, committed and pushed on its own. It is for machines that hold no
// cloud credential but can push to Git, such as an Amp orb, whose git is signed in with a
// short-lived token for the thread's creator. It is write-once like the bucket sinks: a key
// already in the repository is never replaced, and "already there" is reported as fs.ErrExist.
//
// Credentials never pass through Livy: git asks its own credential helper, the remote may not
// carry a user or token, and git is never allowed to prompt.
package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config is the sink section's settings, and the folder that holds the sink's own clone.
type Config struct {
	Remote string // https://host/owner/repo.git, or an absolute path to a repository
	Branch string // default main
	Dir    string // absolute; holds one clone per remote, kept between passes
}

// Sink pushes objects to Config.Remote.
type Sink struct{ c Config }

const (
	defaultBranch = "main"
	// pushAttempts covers another machine pushing between our fetch and our push; keys are
	// content hashes, so a retry never conflicts, it only needs the newer tip.
	pushAttempts = 3
	localBranch  = "livy-sink"
	author       = "livy"
	authorEmail  = "livy@localhost"
)

// New checks the remote and folder. The clone is made on the first Put.
func New(c Config) (*Sink, error) {
	if c.Branch == "" {
		c.Branch = defaultBranch
	}
	if err := checkRemote(c.Remote); err != nil {
		return nil, err
	}
	if c.Dir == "" || !filepath.IsAbs(c.Dir) {
		return nil, fmt.Errorf("the git sink's clone folder must be absolute, got %q", c.Dir)
	}
	// One clone per remote: a ledger sink and a prompt sink on two repositories never share one.
	sum := sha256.Sum256([]byte(c.Remote))
	c.Dir = filepath.Join(c.Dir, hex.EncodeToString(sum[:6]))
	if strings.HasPrefix(c.Branch, "-") || strings.ContainsAny(c.Branch, " :~^?*[\\") {
		return nil, fmt.Errorf("sink.branch %q is not a branch name", c.Branch)
	}
	return &Sink{c: c}, nil
}

// checkRemote accepts an https URL with no user, password, query or fragment, or an absolute
// path. A token in the URL would land in .git/config and in every error message.
func checkRemote(remote string) error {
	if filepath.IsAbs(remote) {
		return nil
	}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" {
		return fmt.Errorf("sink.remote must be an https URL or an absolute path, got %q", redact(remote))
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("sink.remote must not carry a user, token, query or fragment: git's credential helper signs in")
	}
	return nil
}

// redact keeps a refused remote's scheme and host only, in case it carried a token.
func redact(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return "(unparseable)"
}

// URL names where an object lands, for messages.
func (s *Sink) URL(key string) string { return s.c.Remote + " " + s.c.Branch + ":" + key }

// Put commits body at key and pushes it. The clone is reset to the remote's tip before each
// attempt, so it only ever holds what the remote has plus this one object.
func (s *Sink) Put(ctx context.Context, key string, body []byte) error {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return fmt.Errorf("refusing object key %q", key)
	}
	if err := s.clone(ctx); err != nil {
		return err
	}
	var last error
	for range pushAttempts {
		exists, err := s.sync(ctx, key)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%s: %w", s.URL(key), fs.ErrExist)
		}
		if err := s.commit(ctx, key, body); err != nil {
			return err
		}
		if last = s.run(ctx, "push", "--quiet", "origin", "HEAD:refs/heads/"+s.c.Branch); last == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: push refused %d times: %w", s.URL(key), pushAttempts, last)
}

// Ready asks the remote for its branches, which proves it is reachable and that git can sign in
// to read it. It writes nothing; the first push proves write access.
func (s *Sink) Ready(ctx context.Context) error {
	return s.git(ctx, "", "ls-remote", "--quiet", "--heads", s.c.Remote)
}

// clone makes the sink's own repository on first use. It is never a checkout anyone works in.
func (s *Sink) clone(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.c.Dir, ".git")); err == nil {
		return s.run(ctx, "remote", "set-url", "origin", s.c.Remote)
	}
	if err := os.MkdirAll(s.c.Dir, 0o700); err != nil {
		return fmt.Errorf("make the sink's clone folder: %w", err)
	}
	if err := s.run(ctx, "init", "--quiet"); err != nil {
		return err
	}
	return s.run(ctx, "remote", "add", "origin", s.c.Remote)
}

// sync points the clone at the remote's tip (or an empty history when the branch does not exist
// yet) and says whether key is already there.
func (s *Sink) sync(ctx context.Context, key string) (exists bool, err error) {
	found, err := s.remoteHasBranch(ctx)
	if err != nil {
		return false, err
	}
	if !found {
		return false, s.startEmpty(ctx)
	}
	if err := s.run(ctx, "fetch", "--quiet", "--depth=1", "origin", "refs/heads/"+s.c.Branch); err != nil {
		return false, err
	}
	if err := s.run(ctx, "checkout", "--quiet", "--force", "-B", localBranch, "FETCH_HEAD"); err != nil {
		return false, err
	}
	if err := s.run(ctx, "clean", "--quiet", "-fdx"); err != nil {
		return false, err
	}
	return s.git(ctx, s.c.Dir, "cat-file", "-e", "HEAD:"+key) == nil, nil
}

// startEmpty starts the branch with no history and nothing staged, for a repository whose
// branch does not exist yet. An earlier unpushed commit is dropped: its segment is still
// waiting in segments/ and is put again.
func (s *Sink) startEmpty(ctx context.Context) error {
	if err := s.run(ctx, "symbolic-ref", "HEAD", "refs/heads/"+localBranch); err != nil {
		return err
	}
	if err := s.run(ctx, "update-ref", "-d", "refs/heads/"+localBranch); err != nil {
		return err
	}
	if err := s.run(ctx, "read-tree", "--empty"); err != nil {
		return err
	}
	return s.run(ctx, "clean", "--quiet", "-fdx")
}

// remoteHasBranch reads `git ls-remote --exit-code`, which exits 2 when no ref matches.
func (s *Sink) remoteHasBranch(ctx context.Context) (bool, error) {
	err := s.run(ctx, "ls-remote", "--quiet", "--exit-code", "--heads", "origin", "refs/heads/"+s.c.Branch)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		return false, nil
	}
	return err == nil, err
}

func (s *Sink) commit(ctx context.Context, key string, body []byte) error {
	if err := s.writeInClone(filepath.FromSlash(key), body); err != nil {
		return err
	}
	if err := s.run(ctx, "add", "--", key); err != nil {
		return err
	}
	return s.run(ctx, "commit", "--quiet", "--no-verify", "-m", "livy: "+key)
}

// writeInClone writes through an os.Root, because the checkout is the remote's tree: a symlink
// committed at an object's folder would otherwise send the write outside the clone.
func (s *Sink) writeInClone(rel string, body []byte) error {
	root, err := os.OpenRoot(s.c.Dir)
	if err != nil {
		return fmt.Errorf("open the sink's clone: %w", err)
	}
	defer func() { _ = root.Close() }() // a directory handle: a close error loses nothing
	if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return fmt.Errorf("make the object's folder in the clone: %w", err)
	}
	if err := root.WriteFile(rel, body, 0o600); err != nil {
		return fmt.Errorf("write the object into the clone: %w", err)
	}
	return nil
}

func (s *Sink) run(ctx context.Context, args ...string) error { return s.git(ctx, s.c.Dir, args...) }

// git runs one git command. Hooks are off, so Livy's own git hooks never record the sink's
// clone; signing is off, since a signing setup may need a key or a prompt this pass cannot give;
// prompting is off, so a missing credential fails at once rather than hanging the upload.
func (s *Sink) git(ctx context.Context, dir string, args ...string) error {
	_, err := s.gitOutput(ctx, dir, args...)
	return err
}

// gitOutput is git with its standard output.
func (s *Sink) gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.name=" + author, "-c", "user.email=" + authorEmail}
	if dir != "" {
		base = append(base, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(gitFreeEnv(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, fmt.Errorf("git %s: %w (%s)", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// gitFreeEnv is this process's environment without the variables that point git at another
// repository: emit started from inside a git hook inherits GIT_DIR, which -C does not override,
// and the sink's forced checkout and clean would then run on that repository.
func gitFreeEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE":
			continue
		}
		out = append(out, kv)
	}
	return out
}
