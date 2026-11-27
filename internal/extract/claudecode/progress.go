package claudecode

import (
	"path/filepath"

	"github.com/0xbew/livy/internal/extract"
)

// Progress is how far the extractor has read each transcript, from its state file.
type Progress struct {
	files map[string]any
}

// LoadProgress reads the extractor's state in dataDir; with none, nothing has been read.
func LoadProgress(dataDir string) Progress {
	s := loadState(filepath.Join(dataDir, stateName), func(string) {})
	return Progress{files: extract.SubMap(s, "files")}
}

// Through is the byte offset the extractor has read the transcript at path (under root) up to,
// for the generation it holds now; false when it has never read this generation.
func (p Progress) Through(root, path string) (int64, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0, false
	}
	fp, err := firstLineHash(path)
	if err != nil || fp == "" {
		return 0, false
	}
	gen, _ := p.files[pathKey(filepath.ToSlash(rel))].(map[string]any)
	return extract.Offset(gen[fp])
}
