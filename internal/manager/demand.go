package manager

import (
	"context"
	"sync"
	"time"
)

// demandTracker starts and stops a stream's input on demand: once anything
// acquires it (a viewer connection, or a heartbeat from a polling output
// like HLS/DASH), it calls start(); once nothing has held or touched it for
// idleTimeout, it calls stop(). start/stop must be safe to call repeatedly
// in strict alternation (start, stop, start, stop, ...) — never concurrently
// with themselves, which this tracker guarantees via its own mutex.
type demandTracker struct {
	start func()
	stop  func()

	idleTimeout time.Duration

	mu           sync.Mutex
	activeConns  int
	lastActivity time.Time
	running      bool
}

func newDemandTracker(ctx context.Context, start, stop func(), idleTimeout time.Duration) *demandTracker {
	d := &demandTracker{start: start, stop: stop, idleTimeout: idleTimeout, lastActivity: time.Now()}
	go d.idleLoop(ctx)
	return d
}

// Acquire marks one long-lived connection (an HTTP-TS client, a WHEP
// viewer) as active, starting the input if it wasn't already running. The
// caller must call the returned release func exactly once, when that
// connection ends.
func (d *demandTracker) Acquire() (release func()) {
	d.mu.Lock()
	d.activeConns++
	d.lastActivity = time.Now()
	if !d.running {
		d.running = true
		d.start()
	}
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			d.activeConns--
			d.lastActivity = time.Now()
			d.mu.Unlock()
		})
	}
}

// Heartbeat marks request/response-style activity (an HLS/DASH playlist or
// segment request) that doesn't represent an ongoing connection: it starts
// the input if needed, but idles out on its own after idleTimeout with no
// further heartbeats, rather than needing a paired release.
func (d *demandTracker) Heartbeat() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastActivity = time.Now()
	if !d.running {
		d.running = true
		d.start()
	}
}

func (d *demandTracker) IsRunning() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

func (d *demandTracker) idleLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.mu.Lock()
			if d.running && d.activeConns == 0 && time.Since(d.lastActivity) > d.idleTimeout {
				d.running = false
				d.stop()
			}
			d.mu.Unlock()
		}
	}
}
