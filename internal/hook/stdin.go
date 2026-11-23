package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/0xbew/livy/internal/pytext"
)

// The worst case has to fit Claude Code's 5 s hook timeout: 0.5 s for the payload to
// start, 0.5 s to finish, the git budget, the lock wait. Claude Code writes the payload and
// closes stdin, so a real hook never waits on these.
const (
	stdinWait  = 500 * time.Millisecond
	stdinRead  = 500 * time.Millisecond
	stdinChunk = 65536
)

// payload is what the hook read: the object (nil when none), its size in bytes as decoded
// with replacement, and why it could not be used ("" when it could, or when none came).
type payload struct {
	obj     map[string]any
	size    int
	problem string
}

// readChunk is one read of stdin, or its end (eof) with the error that ended it, if any.
type readChunk struct {
	b   []byte
	eof bool
	err error
}

// pump reads f to its end in chunks, then sends the end; EOF is not an error.
func pump(f *os.File, ch chan<- readChunk) {
	for {
		buf := make([]byte, stdinChunk)
		n, err := f.Read(buf)
		if n > 0 {
			ch <- readChunk{b: buf[:n]}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			ch <- readChunk{eof: true, err: err}
			return
		}
	}
}

// readStdin reads the hook payload without ever blocking: an open pipe that carries no
// data and never closes gets stdinWait to start, and a payload that never ends is cut at
// stdinRead (and then fails to parse, which is said).
func readStdin(f *os.File) payload {
	if f == nil {
		return payload{}
	}
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice != 0 {
		return payload{} // a terminal, or /dev/null: nothing offered
	}
	ch := make(chan readChunk, 64)
	go pump(f, ch)
	var data []byte
	select {
	case c := <-ch:
		if c.eof {
			return parseOrFail(data, c.err)
		}
		data = append(data, c.b...)
	case <-time.After(stdinWait):
		return payload{} // nothing offered; not an error
	}
	deadline := time.After(stdinRead)
	for {
		select {
		case c := <-ch:
			if c.eof {
				return parseOrFail(data, c.err)
			}
			data = append(data, c.b...)
		case <-deadline:
			return parse(data)
		}
	}
}

// parseOrFail is parse, or the read error when nothing was read.
func parseOrFail(data []byte, err error) payload {
	if err != nil && len(data) == 0 {
		return payload{problem: "stdin unreadable: " + err.Error()}
	}
	return parse(data)
}

func parse(data []byte) payload {
	raw := pytext.DecodeReplace(data)
	p := payload{size: len(raw)}
	if pytext.Strip(raw) == "" {
		return p // no payload is normal, not an error
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		p.problem = "stdin was not valid JSON"
		return p
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		p.problem = "stdin was not valid JSON"
		return p
	}
	obj, ok := v.(map[string]any)
	if !ok {
		p.problem = "stdin JSON was " + pytext.TypeName(v) + ", expected an object"
		return p
	}
	p.obj = obj
	return p
}
