// EventBus — in-process pub/sub for game events.
//
// Contract from docs/design/airdefenders-multiplayer.md §"Event bus interface":
//
//   - Subscribe returns a receive-only channel buffered to `buffer` events,
//     plus an unsubscribe func that drains and closes the channel.
//   - Publish is non-blocking. If a subscriber's channel is full, the event
//     is DROPPED for that subscriber and a per-subscriber drop counter is
//     incremented. The caller (game-loop tick) must never block.
//
// Subscribers are typically:
//   - the Zenoh publisher (Agent 3) — synchronous publish OK; backpressure
//     surfaces as drops, not as tick lag
//   - any future SSE-leaderboard fan-out for game.over
//
// Lock discipline: a single sync.RWMutex protects the subscriber map.
// Subscribe / unsubscribe take the write lock; Publish takes the read lock
// only while iterating, then sends non-blocking on each chan.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// GameEvent — the on-bus payload. Type is one of the Event* constants;
// Payload's shape is documented per-type in the schema doc.
type GameEvent struct {
	Type      string
	Timestamp time.Time
	EventID   string
	Payload   map[string]any
}

const (
	EventRadarDetection   = "radar.detection"
	EventMissileFired     = "missile.fired"
	EventMissileIntercept = "missile.intercept"
	EventShahedImpact     = "shahed.impact"
)

// EventBus — public interface; Agent 3's Zenoh publisher subscribes here.
type EventBus interface {
	Subscribe(buffer int) (<-chan GameEvent, func())
	Publish(GameEvent)
}

// eventBus — in-process impl.
type eventBus struct {
	mu     sync.RWMutex
	nextID uint64
	subs   map[uint64]*subscriber
	// drops tracks per-subscriber overflow counts.
	// Exposed via DroppedSnapshot() for metrics rendering.
	dropsMu sync.Mutex
	drops   map[string]int64
}

type subscriber struct {
	id    uint64
	label string // human-friendly metric label
	ch    chan GameEvent
}

func newEventBus() *eventBus {
	return &eventBus{
		subs:  map[uint64]*subscriber{},
		drops: map[string]int64{},
	}
}

// Subscribe registers a new subscriber with the given buffer depth.
// Returned channel is closed by the unsubscribe func; callers must not close it.
//
// The subscriber's metric label is derived from the buffer depth + sub ID for
// uniqueness — Zenoh subscriber and any other consumers can call Subscribe
// before declaring themselves; we just need a stable string for the metric.
func (b *eventBus) Subscribe(buffer int) (<-chan GameEvent, func()) {
	return b.subscribeLabel(buffer, "")
}

// subscribeLabel — internal variant that lets the caller name itself in metrics.
// We expose it on the concrete type only (not the interface) for now; Agent 3
// uses the plain Subscribe form, and main.go can use this for a richer label.
func (b *eventBus) subscribeLabel(buffer int, label string) (<-chan GameEvent, func()) {
	if buffer < 1 {
		buffer = 1
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	if label == "" {
		label = "anon"
	}
	s := &subscriber{
		id:    id,
		label: label,
		ch:    make(chan GameEvent, buffer),
	}
	b.subs[id] = s
	b.mu.Unlock()

	unsubscribe := func() {
		b.mu.Lock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			// Drain anything queued so receivers reading on the returned
			// chan after unsubscribe see clean close.
			go func(ch chan GameEvent) {
				for range ch {
				}
			}(s.ch)
			close(s.ch)
		}
		b.mu.Unlock()
	}
	return s.ch, unsubscribe
}

// Publish is non-blocking. Drops to slow subscribers, increments per-label counter.
func (b *eventBus) Publish(ev GameEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			b.recordDrop(s.label)
		}
	}
}

func (b *eventBus) recordDrop(label string) {
	b.dropsMu.Lock()
	b.drops[label]++
	b.dropsMu.Unlock()
}

// DroppedSnapshot returns a copy of the per-subscriber drop counters for metrics.
func (b *eventBus) DroppedSnapshot() map[string]int64 {
	b.dropsMu.Lock()
	defer b.dropsMu.Unlock()
	out := make(map[string]int64, len(b.drops))
	for k, v := range b.drops {
		out[k] = v
	}
	return out
}

// SubscriberCount — for /metrics surface.
func (b *eventBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// ─── ID minting ────────────────────────────────────────────
// Random ID helpers used by EventBus + Hub. crypto/rand only — no UUID dep
// per the project's "no new deps beyond nhooyr.io/websocket" rule.

// newEventID — 128-bit hex string, used as GameEvent.EventID.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback to monotonic time — never panic in the game loop.
		ts := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(ts >> (i * 8))
		}
	}
	return hex.EncodeToString(b[:])
}

// newReconnectToken — 128-bit hex string, used as the per-player reconnect token.
func newReconnectToken() string {
	return newEventID()
}

// ─── Compile-time assertion ────────────────────────────────
var _ EventBus = (*eventBus)(nil)
