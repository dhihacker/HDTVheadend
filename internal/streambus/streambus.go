// Package streambus provides a simple in-process broadcast hub used to fan
// a single stream's data out to any number of output subscribers (HTTP-TS
// clients, UDP senders, etc).
package streambus

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"hdtvheadend/internal/tsutil"
)

// maxGopCache bounds gopCache: a source whose PAT never repeats (or
// repeats only after an unusually long gap) just stops getting the
// instant-join optimization once its GOP would exceed this, rather than
// growing this buffer unboundedly. Generous enough for several seconds
// even at a high bitrate (e.g. ~8s at 8Mbit/s).
const maxGopCache = 8 << 20 // 8MB

// Bus fans byte chunks written by a single input to any number of
// subscribers. Each subscriber gets its own buffered channel; a slow
// subscriber drops data (via a bounded channel) rather than blocking the
// input or other subscribers.
type Bus struct {
	mu   sync.Mutex
	subs map[int]chan []byte
	next int

	totalBytes atomic.Uint64
	rateBits   atomic.Uint64 // float64 bits; updated periodically by sampleRate

	closeOnce sync.Once
	stopCh    chan struct{}
	closed    bool

	// gopCache holds published bytes since the most recently published
	// PAT (PID 0x0000) packet, so a new subscriber can be seeded with an
	// instantly-usable starting point instead of whatever happens to be
	// flowing at the exact moment they join. For a source with a long
	// keyframe interval, joining mid-GOP otherwise means the new viewer's
	// player sits there undecodable for however long is left until the
	// next keyframe — which measured against one real source was a full
	// 10 seconds. Real-world TS muxers conventionally repeat PAT/PMT at
	// or near each keyframe (this project's own tsmux.Muxer explicitly
	// does, matching standard broadcast practice), so "since the last
	// PAT" is a reliable, purely TS-structural proxy for "since the last
	// safe join point" — this package never needs to understand H.264 or
	// keyframes at all to provide this.
	gopCache []byte
}

func New() *Bus {
	b := &Bus{subs: make(map[int]chan []byte), stopCh: make(chan struct{})}
	go b.sampleRate()
	return b
}

// sampleRate periodically recomputes the publish rate from totalBytes and
// caches it, so RateBps is a plain, side-effect-free read: any number of
// callers, at any frequency, see the same (recent) number, unlike a
// "since you last asked" counter that different callers would otherwise
// steal samples from out from under each other.
func (b *Bus) sampleRate() {
	const interval = time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	last := b.totalBytes.Load()
	lastAt := time.Now()
	for {
		select {
		case <-b.stopCh:
			return
		case now := <-ticker.C:
			total := b.totalBytes.Load()
			elapsed := now.Sub(lastAt).Seconds()
			var rate float64
			if elapsed > 0 {
				rate = float64(total-last) / elapsed
			}
			b.rateBits.Store(math.Float64bits(rate))
			last, lastAt = total, now
		}
	}
}

// Subscribe returns a channel of stream chunks and an unsubscribe func.
// chanBuf controls how many pending chunks may queue before new writes to
// this subscriber are dropped.
func (b *Bus) Subscribe(chanBuf int) (<-chan []byte, func()) {
	if chanBuf <= 0 {
		chanBuf = 256
	}
	ch := make(chan []byte, chanBuf)

	b.mu.Lock()
	if b.closed {
		// A stream being edited/removed calls Stop (which Closes its old
		// Bus) but can't unregister its old output paths from the shared
		// HTTP mux (net/http's ServeMux has no such API) — so a client
		// still holding an old URL, or a fresh request to it, can reach
		// this point after Close. Returning an already-closed channel
		// makes that request end cleanly right away (the httpts handler's
		// `chunk, ok := <-ch; if !ok { return }` fires immediately)
		// instead of subscribing to a bus that will never publish or
		// close again, which would otherwise just hang forever.
		b.mu.Unlock()
		closedCh := make(chan []byte)
		close(closedCh)
		return closedCh, func() {}
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	if len(b.gopCache) > 0 {
		// Seed with the current GOP-so-far before this subscriber can
		// receive anything live, so ordering stays correct — Publish
		// blocks on the same lock, so no live chunk can land ahead of
		// this seed. One send regardless of the cache's byte size: a Go
		// channel's buffer is sized in slots, not bytes, so this can't
		// overflow a fresh channel's buffer on its own.
		seed := append([]byte(nil), b.gopCache...)
		select {
		case ch <- seed:
		default:
		}
	}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsub
}

// Publish fans a chunk out to all current subscribers. The chunk must not
// be mutated by the caller afterwards.
func (b *Bus) Publish(chunk []byte) {
	b.totalBytes.Add(uint64(len(chunk)))
	b.mu.Lock()
	defer b.mu.Unlock()

	b.updateGopCache(chunk)

	for _, ch := range b.subs {
		select {
		case ch <- chunk:
		default:
			// Subscriber too slow; drop this chunk for them.
		}
	}
}

// updateGopCache maintains gopCache: restart it at the first PAT found in
// chunk, otherwise keep appending to whatever's already accumulated since
// the last one (or leave it empty if no PAT has been seen yet at all).
// Must be called with b.mu held.
func (b *Bus) updateGopCache(chunk []byte) {
	if patIdx := indexOfPAT(chunk); patIdx >= 0 {
		b.gopCache = append([]byte(nil), chunk[patIdx:]...)
		return
	}
	if len(b.gopCache) == 0 {
		return
	}
	if len(b.gopCache)+len(chunk) > maxGopCache {
		b.gopCache = nil // pathological source; stop rather than growing forever
		return
	}
	b.gopCache = append(b.gopCache, chunk...)
}

// indexOfPAT returns the byte offset of the first TS packet in chunk whose
// PID is 0x0000 (PAT), or -1 if none is found.
func indexOfPAT(chunk []byte) int {
	for i := 0; i+tsutil.PacketSize <= len(chunk); i += tsutil.PacketSize {
		pkt := chunk[i : i+tsutil.PacketSize]
		if pkt[0] != tsutil.SyncByte {
			continue
		}
		if tsutil.PID(pkt) == 0x0000 {
			return i
		}
	}
	return -1
}

// Subscribers returns the current subscriber count.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// RateBps returns the publish rate in bytes/sec, averaged over roughly the
// last second. Side-effect-free: safe to call any number of times, from
// any number of callers, at any frequency, including twice in a row on the
// same Bus (as happens when a stream has no CA and RawBus/Bus alias the
// same instance).
func (b *Bus) RateBps() float64 {
	return math.Float64frombits(b.rateBits.Load())
}

// Close disconnects all current subscribers and stops the background rate
// sampler.
func (b *Bus) Close() {
	b.closeOnce.Do(func() { close(b.stopCh) })
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.gopCache = nil // don't keep serving a frozen snapshot to late subscribers
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
