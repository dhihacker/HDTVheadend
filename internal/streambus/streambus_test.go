package streambus

import (
	"testing"
	"time"
)

// tsPacket builds a minimal 188-byte TS packet with the given PID, filled
// with fill for the remaining payload bytes (so packets are distinguishable
// in test assertions).
func tsPacket(pid uint16, fill byte) []byte {
	pkt := make([]byte, 188)
	pkt[0] = 0x47
	pkt[1] = byte(pid >> 8 & 0x1f)
	pkt[2] = byte(pid & 0xff)
	pkt[3] = 0x10 // payload present, no adaptation field, cc=0
	for i := 4; i < 188; i++ {
		pkt[i] = fill
	}
	return pkt
}

func TestBusGopCacheSeedsNewSubscriber(t *testing.T) {
	b := New()
	defer b.Close()

	pat := tsPacket(0x0000, 0xAA)
	pmt := tsPacket(0x1000, 0xBB)
	video1 := tsPacket(0x0100, 0xCC)
	video2 := tsPacket(0x0100, 0xDD)

	// Simulate one GOP: PAT+PMT+two video packets published as one chunk.
	b.Publish(append(append(append(pat, pmt...), video1...), video2...))

	ch, unsub := b.Subscribe(8)
	defer unsub()

	select {
	case seed := <-ch:
		want := len(pat) + len(pmt) + len(video1) + len(video2)
		if len(seed) != want {
			t.Fatalf("seed length = %d, want %d", len(seed), want)
		}
		if seed[0] != 0x47 || seed[4] != 0xAA {
			t.Fatalf("seed doesn't start at the PAT packet: %x", seed[:8])
		}
	case <-time.After(time.Second):
		t.Fatal("new subscriber got nothing; expected a GOP-cache seed")
	}
}

func TestBusGopCacheAccumulatesUntilNextPAT(t *testing.T) {
	b := New()
	defer b.Close()

	b.Publish(tsPacket(0x0000, 1)) // PAT: starts the cache
	b.Publish(tsPacket(0x0100, 2)) // video: appended
	b.Publish(tsPacket(0x0100, 3)) // video: appended

	if got := len(b.gopCache); got != 188*3 {
		t.Fatalf("gopCache length = %d, want %d", got, 188*3)
	}

	// A new PAT (next GOP) should discard everything before it.
	b.Publish(tsPacket(0x0000, 4))
	if got := len(b.gopCache); got != 188 {
		t.Fatalf("gopCache length after new PAT = %d, want %d (reset)", got, 188)
	}
	if b.gopCache[4] != 4 {
		t.Fatalf("gopCache doesn't reflect the new PAT's chunk")
	}
}

func TestBusGopCacheEmptyBeforeAnyPAT(t *testing.T) {
	b := New()
	defer b.Close()

	b.Publish(tsPacket(0x0100, 1)) // video with no PAT ever seen yet
	if len(b.gopCache) != 0 {
		t.Fatalf("gopCache should stay empty until the first PAT; got %d bytes", len(b.gopCache))
	}

	ch, unsub := b.Subscribe(8)
	defer unsub()
	select {
	case <-ch:
		t.Fatal("subscriber should not have been seeded with anything")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBusGopCacheCapsSize(t *testing.T) {
	b := New()
	defer b.Close()

	b.Publish(tsPacket(0x0000, 1)) // start a GOP
	big := make([]byte, maxGopCache+188)
	b.Publish(big) // no PAT inside; pushes the cache past the cap
	if b.gopCache != nil {
		t.Fatalf("gopCache should have been dropped once it exceeded the cap; got %d bytes", len(b.gopCache))
	}
}

func TestBusSubscribeAfterCloseEndsImmediately(t *testing.T) {
	// Reproduces a stream-edit scenario: the old Bus is Closed (its
	// output paths can't be unregistered from the shared HTTP mux), and a
	// client still holding (or freshly requesting) an old output URL
	// subscribes to it afterwards. That must end right away, not hang
	// forever, and must not still hand out a frozen GOP-cache snapshot.
	b := New()
	b.Publish(tsPacket(0x0000, 1)) // give it a GOP cache to make sure Close clears it
	b.Close()

	ch, unsub := b.Subscribe(8)
	defer unsub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected an already-closed channel, got a value instead")
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe after Close hung instead of returning a closed channel")
	}
}

func TestBusPublishSubscribeOrderingUnaffected(t *testing.T) {
	// Sanity check the seed doesn't interfere with subsequently published
	// live chunks still arriving, in order, after it.
	b := New()
	defer b.Close()

	b.Publish(tsPacket(0x0000, 1))
	ch, unsub := b.Subscribe(8)
	defer unsub()

	live := tsPacket(0x0100, 9)
	b.Publish(live)

	seed := <-ch
	if len(seed) != 188 || seed[4] != 1 {
		t.Fatalf("unexpected seed: %x", seed[:8])
	}
	got := <-ch
	if len(got) != 188 || got[4] != 9 {
		t.Fatalf("unexpected live chunk: %x", got[:8])
	}
}
