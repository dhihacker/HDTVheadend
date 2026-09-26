// Package failover runs an ordered list of input sources against one
// streambus, automatically advancing to the next source when the active
// one fails, and supports pinning a specific source (manual switching).
package failover

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hdtvheadend/internal/config"
	"hdtvheadend/internal/streambus"
)

// RunFunc runs one input source until it fails or ctx is canceled.
type RunFunc func(ctx context.Context, in config.Input, bus *streambus.Bus) error

// Supervisor drives failover across an ordered list of sources: index 0 is
// the primary, the rest are backups tried in order. After exhausting the
// list it wraps back to the start (with a backoff between attempts), so a
// stream recovers automatically once a source comes back.
type Supervisor struct {
	sources []config.Input
	run     RunFunc
	bus     *streambus.Bus

	forced atomic.Int32 // -1 = automatic; otherwise a pinned source index

	mu           sync.Mutex
	active       int
	lastErr      error
	cancelActive context.CancelFunc
}

func NewSupervisor(sources []config.Input, run RunFunc, bus *streambus.Bus) *Supervisor {
	s := &Supervisor{sources: sources, run: run, bus: bus}
	s.forced.Store(-1)
	return s
}

// Run drives the supervisor until ctx is canceled. onEvent, if non-nil, is
// called twice per source attempt: once with a nil err right before that
// source starts, and once with the error it stopped with (nil only if ctx
// was canceled) right after.
func (s *Supervisor) Run(ctx context.Context, onEvent func(index int, err error)) error {
	if len(s.sources) == 0 {
		return fmt.Errorf("failover: no input sources configured")
	}

	idx := 0
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if f := int(s.forced.Load()); f >= 0 && f < len(s.sources) {
			idx = f
		}

		attemptCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.active = idx
		s.cancelActive = cancel
		s.mu.Unlock()
		if onEvent != nil {
			onEvent(idx, nil)
		}

		err := s.run(attemptCtx, s.sources[idx], s.bus)
		cancel()

		if ctx.Err() != nil {
			return ctx.Err()
		}

		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		if onEvent != nil {
			onEvent(idx, err)
		}

		// A source that was pinned and then failed on its own shouldn't
		// stay pinned forever; fall back to automatic rotation.
		if int(s.forced.Load()) == idx {
			s.forced.Store(-1)
		}
		idx = (idx + 1) % len(s.sources)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// SwitchTo pins the supervisor to source index i (0 = primary, 1 = first
// backup, ...) and immediately interrupts whatever source is currently
// running so the switch takes effect right away. A negative index returns
// to automatic failover (without interrupting the current source).
func (s *Supervisor) SwitchTo(i int) error {
	if i >= 0 && i >= len(s.sources) {
		return fmt.Errorf("failover: source index %d out of range (have %d sources)", i, len(s.sources))
	}
	s.forced.Store(int32(i))
	if i < 0 {
		return nil
	}
	s.mu.Lock()
	cancel := s.cancelActive
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Status reports the currently active source index, whether it was
// manually pinned, the total number of configured sources, and the last
// error recorded from a failed source (nil if none has failed yet).
func (s *Supervisor) Status() (active int, pinned bool, total int, lastErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.forced.Load() >= 0, len(s.sources), s.lastErr
}
