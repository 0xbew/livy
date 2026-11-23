package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A skill folder is hashed inside a hook that Claude Code kills after 5 s, so a folder past
// these bounds (a vendored dependency tree) gets no tree hash rather than a killed hook.
const (
	maxTreeFiles = 1000
	maxTreeBytes = 32 << 20
)

// ErrTreeTooLarge is a skill folder past the bounds a hook can hash in time.
var ErrTreeTooLarge = errors.New("skill folder too large to hash in a hook")

// Hashes are a skill folder's two hashes, hex.
type Hashes struct {
	SkillMD string // sha256 of SKILL.md's bytes, the value scripts/copies.json records
	Tree    string // TreeSHA256 of the whole folder; "" when TreeErr is set
	TreeErr error
}

// HashDir hashes dir's SKILL.md and, separately, the whole folder: a folder too large to
// hash still gets its SKILL.md hash.
func HashDir(dir string) (Hashes, error) {
	md, err := fileSHA256(filepath.Join(dir, skillFile))
	if err != nil {
		return Hashes{}, err
	}
	h := Hashes{SkillMD: md}
	h.Tree, h.TreeErr = TreeSHA256(dir)
	return h, nil
}

// TreeSHA256 is the sha256 of the shasum-style listing of every regular file under dir
// (dir itself may be a symlink; links inside it are not followed), one
// "<sha256>  <slash path relative to dir>\n" per file, sorted by path bytes. The same value
// comes from:
//
//	cd -P DIR && find . -type f | sed 's|^\./||' | LC_ALL=C sort | while read -r f; do shasum -a 256 "$f"; done | shasum -a 256
func TreeSHA256(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("skill folder: %w", err)
	}
	dir = resolved
	files, err := regularFiles(dir)
	if err != nil {
		return "", err
	}
	var listing strings.Builder
	var total int64
	for _, f := range files {
		total += f.size
		if total > maxTreeBytes {
			return "", ErrTreeTooLarge
		}
		sum, err := fileSHA256(filepath.Join(dir, filepath.FromSlash(f.rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&listing, "%s  %s\n", sum, f.rel)
	}
	return HashString(listing.String()), nil
}

// treeFile is one regular file under a skill folder.
type treeFile struct {
	rel  string
	size int64
}

// regularFiles is every regular file under dir, sorted by slash path.
func regularFiles(dir string) ([]treeFile, error) {
	var files []treeFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("skill folder: %w", err)
		}
		if len(files) == maxTreeFiles {
			return ErrTreeTooLarge
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return fmt.Errorf("skill folder: %w", err)
		}
		files = append(files, treeFile{rel: filepath.ToSlash(rel), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk skill folder: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("open skill file: %w", err)
	}
	defer func() { _ = f.Close() }() // opened read-only: a close error loses nothing
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read skill file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
