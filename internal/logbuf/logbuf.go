// Package logbuf provides a small in-memory ring buffer that can be used as
// an additional io.Writer target for the standard logger, so recent log
// lines can be served over the web UI without reading back the process's
// own stdout/stderr.
package logbuf

import (
	"strings"
	"sync"
	"time"
)

type Line struct {
	Seq  int64
	Time time.Time
	Text string
}

// Ring keeps the most recent N log lines written to it.
type Ring struct {
	mu      sync.Mutex
	lines   []Line
	cap     int
	next    int
	full    bool
	lastSeq int64
}

func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{lines: make([]Line, capacity), cap: capacity}
}

// Write implements io.Writer. A single Write call may contain multiple
// newline-terminated log lines (as the standard logger does when combining
// its prefix and message); each is stored separately.
func (r *Ring) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		r.lastSeq++
		r.lines[r.next] = Line{Seq: r.lastSeq, Time: now, Text: line}
		r.next = (r.next + 1) % r.cap
		if r.next == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Lines returns the buffered lines in chronological order (oldest first).
func (r *Ring) Lines() []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.linesLocked()
}

func (r *Ring) linesLocked() []Line {
	if !r.full {
		out := make([]Line, r.next)
		copy(out, r.lines[:r.next])
		return out
	}
	out := make([]Line, r.cap)
	copy(out, r.lines[r.next:])
	copy(out[r.cap-r.next:], r.lines[:r.next])
	return out
}

// LinesSince returns buffered lines with Seq > since, oldest first. Pass 0
// to get everything currently buffered. If the buffer has wrapped past
// since entirely (the caller fell far enough behind that some lines were
// overwritten), it just returns everything currently available rather
// than erroring — the caller can tell from the first returned Seq whether
// anything was skipped.
func (r *Ring) LinesSince(since int64) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()

	all := r.linesLocked()
	for i, line := range all {
		if line.Seq > since {
			return all[i:]
		}
	}
	return nil
}

// LastSeq returns the sequence number of the most recently written line
// (0 if nothing has been written yet), so a caller can start tailing from
// "now" without first fetching the whole buffer.
func (r *Ring) LastSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSeq
}
