// EventBus tests — focused on the documented drop-on-slow-subscriber behaviour.
package main

import (
	"sync"
	"testing"
	"time"
)

func TestEventBus_DropOnSlowSubscriber(t *testing.T) {
	bus := newEventBus()
	// Buffer of 2; we publish 5 without draining → 3 drops expected.
	ch, unsub := bus.subscribeLabel(2, "slow")
	defer unsub()

	for i := 0; i < 5; i++ {
		bus.Publish(GameEvent{Type: EventMissileFired, Timestamp: time.Now()})
	}

	got := bus.DroppedSnapshot()["slow"]
	if got != 3 {
		t.Fatalf("expected 3 drops on slow subscriber; got %d", got)
	}

	// Drain — should yield exactly the 2 buffered events.
	drained := 0
drain:
	for {
		select {
		case <-ch:
			drained++
		default:
			break drain
		}
	}
	if drained != 2 {
		t.Fatalf("expected to drain 2 buffered events; got %d", drained)
	}
}

func TestEventBus_FastSubscriberNeverDrops(t *testing.T) {
	bus := newEventBus()
	// Buffer must be >= N so the synchronous publisher loop doesn't
	// race the consumer goroutine — Publish is non-blocking, so without
	// adequate buffer the publisher overflows before the consumer drains.
	const N = 50
	ch, unsub := bus.subscribeLabel(N, "fast")
	defer unsub()

	done := make(chan struct{})
	go func() {
		count := 0
		for range ch {
			count++
			if count == N {
				close(done)
				return
			}
		}
	}()

	for i := 0; i < N; i++ {
		bus.Publish(GameEvent{Type: EventShahedImpact})
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fast subscriber timed out")
	}
	if got := bus.DroppedSnapshot()["fast"]; got != 0 {
		t.Fatalf("expected 0 drops on fast subscriber; got %d", got)
	}
}

func TestEventBus_MultipleSubscribersIndependent(t *testing.T) {
	bus := newEventBus()
	// A drains fast (buffer holds all 10); B never drains (buffer 1).
	// A buffer must be >= publish count so we don't race the publisher's
	// tight loop against A's drain goroutine — Publish is non-blocking,
	// so without a draining receiver the publisher fires all 10 events
	// without yielding and A's buffer-1 chan overflows after the first.
	chA, unsubA := bus.subscribeLabel(16, "A")
	defer unsubA()
	chB, unsubB := bus.subscribeLabel(1, "B")
	defer unsubB()

	// Drain only A; B fills up and drops.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			<-chA
		}
	}()

	for i := 0; i < 10; i++ {
		bus.Publish(GameEvent{Type: EventRadarDetection})
	}
	wg.Wait()

	drops := bus.DroppedSnapshot()
	if drops["A"] != 0 {
		t.Errorf("A should have zero drops; got %d", drops["A"])
	}
	if drops["B"] == 0 {
		t.Errorf("B should have drops > 0; got 0")
	}
	// Drain whatever B has so the deferred unsub doesn't deadlock.
	for len(chB) > 0 {
		<-chB
	}
}

func TestEventBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := newEventBus()
	ch, unsub := bus.subscribeLabel(4, "transient")

	bus.Publish(GameEvent{Type: EventMissileFired})
	unsub()

	// After unsub, the channel should drain and then close.
	timeout := time.After(time.Second)
	closed := false
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-timeout:
			t.Fatal("channel never closed after unsubscribe")
		}
	}
}
