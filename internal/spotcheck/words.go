package spotcheck

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/0xbew/livy/internal/pytext"
)

const (
	shingleWords = 8
	// A whole prompt shorter than a shingle is still matched, by containment, down to this
	// many words. Below it ("ok", "go ahead") a match proves nothing.
	shortPromptMinWords = 3
)

// words lowercases text, turns markdown marks into spaces and splits on whitespace.
func words(text string) []string {
	text = strings.ToLower(text)
	var b strings.Builder
	for _, r := range text {
		switch r {
		case '*', '`', '#', '>', '|', '_', '~', '•', '▪', '●', '◦', '-':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.FieldsFunc(b.String(), pytext.IsSpace)
}

func shingles(w []string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i+shingleWords <= len(w); i++ {
		out[strings.Join(w[i:i+shingleWords], " ")] = true
	}
	return out
}

// OwnerPrompts is the engineer's own prompts in a session as word lists; ok false when
// the transcript is unreadable. The whole file is streamed: the prompts that set a task up
// come first. A human prompt is a user record whose content is text; tool results are not.
func OwnerPrompts(path string) (prompts [][]string, ok bool) {
	if path == "" {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := rd.ReadString('\n')
		if strings.Contains(line, `"user"`) {
			prompts = appendPrompts(prompts, line)
		}
		if err != nil {
			if err != io.EOF {
				return nil, false
			}
			return prompts, true
		}
	}
}

func appendPrompts(prompts [][]string, line string) [][]string {
	var rec map[string]any
	if json.Unmarshal([]byte(line), &rec) != nil {
		return prompts
	}
	if rec["type"] != "user" || pytext.Truthy(rec["isMeta"]) || pytext.Truthy(rec["isSidechain"]) {
		return prompts
	}
	msg, _ := rec["message"].(map[string]any)
	var texts []string
	switch c := msg["content"].(type) {
	case string:
		texts = []string{c}
	case []any:
		for _, b := range c {
			if m, isMap := b.(map[string]any); isMap && m["type"] == "text" {
				s, _ := m["text"].(string)
				texts = append(texts, s)
			}
		}
	}
	for _, t := range texts {
		if w := words(t); len(w) > 0 {
			prompts = append(prompts, w)
		}
	}
	return prompts
}

// RepeatsOwner reports whether text repeats one of the prompts: a shared run of eight
// words, or a whole short prompt contained in it.
func RepeatsOwner(text string, prompts [][]string) bool {
	w := words(text)
	joined := " " + strings.Join(w, " ") + " "
	brief := shingles(w)
	for _, p := range prompts {
		switch {
		case len(p) >= shingleWords:
			for s := range shingles(p) {
				if brief[s] {
					return true
				}
			}
		case len(p) >= shortPromptMinWords:
			if strings.Contains(joined, " "+strings.Join(p, " ")+" ") {
				return true
			}
		}
	}
	return false
}
