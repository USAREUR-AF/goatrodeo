// Multiplayer — Hub + Room + tick loop + roster + reconnect tokens.
//
// One global Room in v1 (`room_id: "global"`). The Room owns a single tick
// goroutine that drains per-player input queues, advances physics, marshals
// the snapshot, and fans out to writer goroutines via per-connection
// buffered chans (drop on full — slow client gets a stale frame but never
// blocks the tick).
//
// Hub responsibilities:
//   - assign monotonic player_id on hello
//   - hold the 30-second reconnect-token map (mutex-guarded)
//   - own the single Room
//   - gate connections via per-IP cap
//
// Lock discipline: Room state is owned by its tick goroutine. Hub roster +
// reconnect tokens take a small mutex (rarely contended).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── Tick rate ─────────────────────────────────────────────

const (
	tickHz    = 30
	tickDur   = time.Second / tickHz
	worldW    = 800
	worldH    = 700
	maxPlayers = 8
	// reconnectWindow — schema doc §"Reconnect token".
	reconnectWindow = 30 * time.Second
	// idleResetWindow — 5min with zero players ⇒ reset to TITLE per §Rooms.
	idleResetWindow = 5 * time.Minute
)

// ─── Player palette ────────────────────────────────────────
// Fixed 8-entry palette, deterministic via (player_id % 8).
// player_id 1 (first joiner) lands on index 1 = legacy single-player green,
// so a solo room looks visually identical to today's game.
// Index 0 is filled with amber for completeness — player_id 0 is never
// assigned (allocator increments from 0 before use).
var playerPalette = [8]string{
	"#ff8844", // 0 — amber (unused; player_id starts at 1)
	"#00ff88", // 1 — bright green (legacy single-player)
	"#44aaff", // 2 — sky-blue
	"#ff44aa", // 3 — pink
	"#ffcc00", // 4 — yellow
	"#aa44ff", // 5 — purple
	"#00ddcc", // 6 — teal
	"#ff5555", // 7 — red
}

// ─── Hub ───────────────────────────────────────────────────

// Hub — singleton owning the one global Room + reconnect-token map.
type Hub struct {
	bus     *eventBus
	db      *sql.DB
	rate    *rateLimiter // share the leaderboard rate limiter for auto-submit
	sseHub  *sseHub      // for SSE-broadcast of auto-submitted score row

	mu              sync.Mutex
	nextPlayerID    uint32
	reconnectTokens map[string]*reconnectSlot // keyed by token

	room *Room

	// per-IP conn cap is tracked here too so ws.go can ask before upgrade
	ipConnCount map[string]int

	// metrics counters
	wsConnections atomic.Int64
	players       atomic.Int64
}

// reconnectSlot — held for up to 30s after a player's WS closes.
type reconnectSlot struct {
	playerID   uint32
	name       string
	color      string
	intercepts uint32
	shots      uint32
	expiresAt  time.Time
	// launcherSlot — the reserved-X-slot the disconnecting player held.
	// Re-applied on a successful reconnect so the player's launcher stays
	// at the same X. Independent of the game-state launcher entry itself
	// (which is preserved across the 30s window — see Room.run's leaveCh
	// branch, which does NOT despawn the launcher; sweepDisconnected does
	// that on slot expiry).
	launcherSlot int
}

// newHub — wires the singleton. Caller must call Start to spin up the tick goroutine.
func newHub(bus *eventBus, db *sql.DB, rate *rateLimiter, sse *sseHub) *Hub {
	h := &Hub{
		bus:             bus,
		db:              db,
		rate:            rate,
		sseHub:          sse,
		reconnectTokens: map[string]*reconnectSlot{},
		ipConnCount:     map[string]int{},
	}
	h.room = newRoom("global", h)
	return h
}

// Start spins the room's tick goroutine + a periodic token-sweep.
// Returns immediately. Call Stop to tear down.
func (h *Hub) Start(ctx context.Context) {
	go h.room.run(ctx)
	go h.tokenSweeper(ctx)
}

func (h *Hub) tokenSweeper(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.expireTokens()
		}
	}
}

func (h *Hub) expireTokens() {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for tok, slot := range h.reconnectTokens {
		if now.After(slot.expiresAt) {
			delete(h.reconnectTokens, tok)
		}
	}
}

// reserveIP — checks + increments the per-IP conn count. Returns false if cap reached.
func (h *Hub) reserveIP(ip string, cap int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ipConnCount[ip] >= cap {
		return false
	}
	h.ipConnCount[ip]++
	return true
}

func (h *Hub) releaseIP(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ipConnCount[ip] > 0 {
		h.ipConnCount[ip]--
	}
	if h.ipConnCount[ip] == 0 {
		delete(h.ipConnCount, ip)
	}
}

// allocatePlayer — assigns or restores a player_id. token may be "" for a fresh joiner.
//
// Returns (playerID, color, name, isReconnect). On reconnect, the prior
// contribution counters (intercepts, shots) are obtainable via the second
// form, allocatePlayerWithCounters, for callers that want to seed them.
func (h *Hub) allocatePlayer(name string, token string) (uint32, string, string, bool) {
	pid, color, n, reconn, _, _, _ := h.allocatePlayerWithCounters(name, token)
	return pid, color, n, reconn
}

// allocatePlayerWithCounters — variant that also returns the reconnect-slot's
// prior intercepts/shots + launcher slot so the caller can carry them onto
// the new Player. Launcher slot of -1 means "fresh joiner — Room.run will
// assign a slot in the joinCh branch."
func (h *Hub) allocatePlayerWithCounters(name string, token string) (
	pid uint32, color string, finalName string, isReconnect bool,
	priorIntercepts uint32, priorShots uint32, priorLauncherSlot int,
) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if token != "" {
		if slot, ok := h.reconnectTokens[token]; ok && time.Now().Before(slot.expiresAt) {
			delete(h.reconnectTokens, token)
			n := slot.name
			if name != "" {
				n = name
			}
			return slot.playerID, slot.color, n, true, slot.intercepts, slot.shots, slot.launcherSlot
		}
	}
	h.nextPlayerID++
	pid = h.nextPlayerID
	color = playerPalette[pid%uint32(len(playerPalette))]
	return pid, color, name, false, 0, 0, -1
}

// retainReconnectToken — called when a WS closes. Stores a 30s slot.
// Returns the token string so ws.go can communicate it (but we generate
// the token at welcome time, not here; this just stashes the slot).
func (h *Hub) retainReconnectToken(token string, p *Player) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reconnectTokens[token] = &reconnectSlot{
		playerID:     p.ID,
		name:         p.Name,
		color:        p.Color,
		intercepts:   p.Intercepts,
		shots:        p.Shots,
		expiresAt:    time.Now().Add(reconnectWindow),
		launcherSlot: p.LauncherSlot,
	}
}

// PlayerCount + ConnCount — for metrics.
func (h *Hub) PlayerCount() int64 { return h.players.Load() }
func (h *Hub) ConnCount() int64   { return h.wsConnections.Load() }

// ─── Player ────────────────────────────────────────────────

// Player — server-side per-connection state.
type Player struct {
	ID         uint32
	Name       string
	Color      string
	JoinedAt   int64 // unix millis
	Intercepts uint32
	Shots      uint32

	// reconnect_token: minted at welcome, persisted on the connection so
	// the disconnect handler can stash it in the Hub's reconnect map.
	ReconnectToken string

	// conn handle — set by ws.go when wiring writer goroutine.
	// Per-player input queue is buffered; the tick goroutine drains it.
	inputs chan InputEvent
	// outbound — Tick goroutine pushes pre-marshaled snapshots/messages here;
	// the writer goroutine drains and sends. Drop on full.
	outbound chan []byte
	// disconnected — set true when the WS closes. The tick goroutine treats
	// this as "do not apply inputs from this player"; the player stays in the
	// roster until the reconnect window expires.
	disconnected bool
	// spaceCooldown — per-player input.fire / input.state_advance cooldown
	// (15 ticks per schema doc §"Reconnect/spaceCooldown" + index.html L776/L1854).
	spaceCooldown int
	// fireWindowStart + fireWindowCount — for the 30/sec rate limit.
	fireWindowStart int64
	fireWindowCount int
	// launcherMoveWindowStart + launcherMoveWindowCount — same scheme,
	// schema doc §"input.launcher_move", capped at 30/sec/player.
	launcherMoveWindowStart int64
	launcherMoveWindowCount int

	// LauncherSlot — the reserved-X-slot this player's launcher occupies.
	// Stable across reconnects within the 30s window. -1 means "no launcher
	// currently owned" (pre-spawn race / post-despawn). Schema doc
	// §"Rooms and session lifecycle".
	LauncherSlot int
}

// ─── Input ─────────────────────────────────────────────────

// InputEvent — one decoded WS frame queued for the tick goroutine.
type InputEvent struct {
	PlayerID  uint32
	ReceivedAt time.Time
	Type      string

	// flat fields — only the relevant ones are populated per Type
	LauncherIdx    int
	Mode           int
	Aim            *Vec2
	InterceptorEID uint32
	RadarIdx       int
	Pos            *Vec2
	PongEcho       string
	Name           string  // for input.set_name
	DX             float64 // for input.launcher_move
}

// Vec2 — world-space coords; matches schema doc.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// ─── Room ──────────────────────────────────────────────────

// Room — owns one game's worth of state. Single tick goroutine; all state
// mutation happens there.
type Room struct {
	id  string
	hub *Hub

	// joins/leaves arrive via these chans; tick goroutine drains them.
	joinCh    chan *Player
	leaveCh   chan uint32 // player_id
	inputCh   chan InputEvent
	// requestSnapshot — synchronous full-snapshot query (used by ws.go's
	// welcome path so we can hand the joiner the latest state without
	// racing the tick goroutine).
	snapReqCh chan chan []byte
	// rosterReqCh — synchronous roster query (welcome path).
	rosterReqCh chan chan []byte

	// game-state — owned exclusively by the tick goroutine.
	g *gameState

	// players in the room (by ID). Owned by tick goroutine.
	players map[uint32]*Player

	// for clean shutdown
	stopOnce sync.Once
}

func newRoom(id string, h *Hub) *Room {
	r := &Room{
		id:          id,
		hub:         h,
		joinCh:      make(chan *Player, 16),
		leaveCh:     make(chan uint32, 16),
		inputCh:     make(chan InputEvent, 1024),
		snapReqCh:   make(chan chan []byte, 8),
		rosterReqCh: make(chan chan []byte, 8),
		players:     map[uint32]*Player{},
	}
	r.g = newGameState(h.bus)
	// Wire attribution: when gameState credits a kill to ownerID, bump the
	// matching player's Intercepts counter. Runs on the tick goroutine, so
	// no lock needed on the players map.
	r.g.attributeFn = func(ownerID uint32, n int) {
		if ownerID == 0 {
			return
		}
		if p, ok := r.players[ownerID]; ok {
			p.Intercepts += uint32(n)
		}
	}
	// Wire wave.summary: gameState collects kills + leaks during COMBAT and
	// flushes via this callback on transitions out (combat→wave_end and
	// combat/wave_end → game_over). Marshalled + broadcast inline since this
	// runs on the tick goroutine.
	r.g.onWaveSummary = func(wave int, kills []killSnapshot, leaked int, shahedPaths, interceptorPaths []pathRecord) {
		frame := r.marshalWaveSummary(wave, kills, leaked, shahedPaths, interceptorPaths)
		r.broadcast(frame)
	}
	return r
}

// Join — async-enqueues a player join. Safe from any goroutine.
func (r *Room) Join(p *Player) {
	r.joinCh <- p
}

// Leave — async-enqueues a player leave (by ID).
func (r *Room) Leave(playerID uint32) {
	r.leaveCh <- playerID
}

// SubmitInput — non-blocking enqueue of an input event. Drops on overflow.
func (r *Room) SubmitInput(ev InputEvent) {
	select {
	case r.inputCh <- ev:
	default:
		droppedInputsInc("queue_full")
	}
}

// Snapshot — returns the marshaled current snapshot. Used by welcome path.
// Blocks until the tick goroutine answers; bounded by tick rate.
func (r *Room) Snapshot(ctx context.Context) []byte {
	out := make(chan []byte, 1)
	select {
	case r.snapReqCh <- out:
	case <-ctx.Done():
		return nil
	}
	select {
	case b := <-out:
		return b
	case <-ctx.Done():
		return nil
	}
}

// RosterJSON — returns the marshaled current roster (`roster.update` payload).
func (r *Room) RosterJSON(ctx context.Context) []byte {
	out := make(chan []byte, 1)
	select {
	case r.rosterReqCh <- out:
	case <-ctx.Done():
		return nil
	}
	select {
	case b := <-out:
		return b
	case <-ctx.Done():
		return nil
	}
}

// run — the tick goroutine. Owns all mutation of r.g + r.players.
func (r *Room) run(ctx context.Context) {
	t := time.NewTicker(tickDur)
	defer t.Stop()

	var (
		tickNum       uint32
		lastActiveAt  = time.Now()
		pausedForIdle bool
	)

	for {
		select {
		case <-ctx.Done():
			r.stopOnce.Do(func() { r.broadcastShutdown() })
			return

		case p := <-r.joinCh:
			r.players[p.ID] = p
			r.hub.players.Add(1)
			// Launcher ownership: fresh joiner gets a new launcher in the
			// first free slot; reconnecting joiner's launcher is already in
			// r.g.launchers (preserved across the 30 s reconnect window),
			// so we just reattach by owner-id.
			if existing := r.g.launcherIdxForOwner(p.ID); existing >= 0 {
				// Reconnect — launcher is still there. Sync the player's
				// recorded slot to whatever the launcher has (defends
				// against any prior slot-state divergence).
				p.LauncherSlot = r.g.launchers[existing].Slot
			} else {
				_, slot, ok := r.g.spawnLauncherForOwner(p.ID)
				if ok {
					p.LauncherSlot = slot
				} else {
					// All 8 slots taken. ws.go's maxPlayers cap should have
					// rejected this connection upstream; if we get here,
					// log and continue with no launcher (input verbs will
					// drop with not_owner).
					log.Printf("room: no free launcher slot for player %d", p.ID)
					p.LauncherSlot = -1
				}
			}
			r.broadcastRoster()
			// Resume if we had paused.
			pausedForIdle = false
			lastActiveAt = time.Now()

		case id := <-r.leaveCh:
			if p, ok := r.players[id]; ok {
				// Mark disconnected (kept in roster). Hub holds reconnect token.
				p.disconnected = true
				// Stash a snapshot of the player's counters into the reconnect
				// slot HERE — running on the tick goroutine — so we don't race
				// the ws-handler's defer.
				r.hub.retainReconnectToken(p.ReconnectToken, p)
				// Stop owning the chans — writer goroutine has exited.
				p.outbound = nil
				p.inputs = nil
				// If reconnect window has lapsed, the periodic sweep below removes them.
				r.hub.players.Add(-1)
				r.broadcastRoster()
			}

		case ev := <-r.inputCh:
			// Tick goroutine: safe to mutate.
			r.applyInput(ev)

		case out := <-r.snapReqCh:
			out <- r.marshalSnapshot(tickNum)

		case out := <-r.rosterReqCh:
			out <- r.marshalRoster()

		case <-t.C:
			tickNum++

			// Are any players connected? If not, pause physics.
			anyConnected := false
			for _, p := range r.players {
				if !p.disconnected {
					anyConnected = true
					break
				}
			}
			if anyConnected {
				lastActiveAt = time.Now()
				pausedForIdle = false
			} else {
				// 5min idle → full reset.
				if !pausedForIdle && time.Since(lastActiveAt) > idleResetWindow {
					r.g.reset()
					pausedForIdle = true
				}
			}

			if !anyConnected {
				// pause — emit nothing, don't advance physics.
				continue
			}

			// Decrement per-player cooldowns (always).
			for _, p := range r.players {
				if p.spaceCooldown > 0 {
					p.spaceCooldown--
				}
			}

			// Advance physics.
			start := time.Now()
			prevPhase := r.g.phase
			gameOverFired := r.g.gameOverFired
			r.g.tick(tickNum)
			tickDurMicro.Add(time.Since(start).Microseconds())
			tickCount.Add(1)

			// CELEBRATION gate: if this tick flipped us into GAME_OVER from
			// COMBAT (and not from CELEBRATION already), check the leaderboard
			// for a top-5 placement; if so, redirect into CELEBRATION for
			// celebrationTicks ticks. GAME_OVER follows when the timer expires.
			if !gameOverFired && r.g.phase == phaseGameOver && prevPhase == phaseCombat {
				if r.shouldCelebrate(r.g.score) {
					r.g.phase = phaseCelebration
					r.g.celebrationTimer = celebrationTicks
				}
			}

			// If we just transitioned to GAME_OVER, build ranks, broadcast game.over,
			// and auto-submit the leaderboard score.
			if !gameOverFired && r.g.phase == phaseGameOver {
				r.g.gameOverFired = true
				go r.onGameOver()
			}

			// Marshal snapshot once + fan-out.
			snap := r.marshalSnapshot(tickNum)
			r.broadcast(snap)

			// Sweep removed-by-disconnect players whose reconnect window lapsed.
			// (The Hub's tokenSweeper also clears the slot; here we just trim the roster.)
			r.sweepDisconnected()
		}
	}
}

// applyInput — called from the tick goroutine.
func (r *Room) applyInput(ev InputEvent) {
	p, ok := r.players[ev.PlayerID]
	if !ok || p.disconnected {
		droppedInputsInc("no_player")
		return
	}
	switch ev.Type {
	case "input.fire":
		// Per-player launcher ownership: server resolves to the sender's
		// owned launcher_idx, ignoring any client-supplied value. The
		// `launcher_idx` field on input.fire is advisory only post-rewrite.
		// Check ownership FIRST so a not-owning sender doesn't consume the
		// per-player fire-rate budget.
		ownedIdx := r.g.launcherIdxForOwner(p.ID)
		if ownedIdx < 0 {
			droppedInputsInc("not_owner")
			return
		}
		// per-player spaceCooldown 15 ticks (schema doc §input table)
		if p.spaceCooldown > 0 {
			return
		}
		if !r.checkFireRate(p) {
			return
		}
		if r.g.fireFromLauncher(ownedIdx, p) {
			p.spaceCooldown = 15
			p.Shots++
		}
	case "input.launcher_switch":
		// Legacy verb — no-op in per-player ownership mode (each player owns
		// exactly one launcher, so there is nothing to switch). Kept on the
		// dispatcher so older clients don't get spammed with malformed-input
		// errors during the change-set roll-forward.
	case "input.fire_mode":
		// Owner-only — players control their own launcher's mode
		// independently. Reverses the round-1 "shared fire_mode" decision.
		if !r.requireOwner(p, ev.LauncherIdx) {
			return
		}
		r.g.setFireMode(ev.LauncherIdx, ev.Mode)
	case "input.resupply":
		if !r.requireOwner(p, ev.LauncherIdx) {
			return
		}
		r.g.triggerResupply(ev.LauncherIdx)
	case "input.steer":
		r.g.steerInterceptor(ev.InterceptorEID, ev.Aim, p.ID)
	case "input.bunker_run":
		r.g.startBunkerRun()
	case "input.camel_release":
		r.g.releaseCamel()
	case "input.launcher_move":
		// Owner-check first so a misaddressed frame doesn't burn the
		// per-player move budget.
		if !r.requireOwner(p, ev.LauncherIdx) {
			return
		}
		// Per-player rate-limit per schema doc §input.launcher_move.
		if !r.checkLauncherMoveRate(p) {
			return
		}
		if !r.g.moveLauncher(ev.LauncherIdx, ev.DX) {
			droppedInputsInc("bad_launcher")
		}
	case "input.radar_place":
		if r.g.phase == phaseEmplacement {
			r.g.placeRadar(ev.RadarIdx, ev.Pos)
		}
	case "input.state_advance":
		if p.spaceCooldown > 0 {
			return
		}
		if r.g.advanceState() {
			p.spaceCooldown = 15
		}
	case "input.set_name":
		// Display-name update. Normalised the same way as leaderboard inputs.
		// Agent 2 calls this just before/at GAME_OVER so the auto-submit uses
		// the right name. Roster is re-broadcast so other clients see the change.
		nn, _ := normaliseName(ev.Name)
		if nn != "" && nn != p.Name {
			p.Name = nn
			r.broadcastRoster()
		}
	case "pong":
		// liveness — handled by the ws.go reader loop's deadline reset.
	}
}

// requireOwner — drops the input if launcherIdx isn't owned by p (or is
// out of bounds). Increments dropped_inputs{reason="not_owner"} on mismatch.
// Returns true if the caller should proceed with the verb.
//
// Used for input.fire_mode / input.resupply / input.launcher_move per the
// per-player ownership contract (schema doc §"Failure modes" + §"WebSocket
// message schema" → input table). input.fire uses launcherIdxForOwner
// directly because its launcher_idx field is advisory only.
func (r *Room) requireOwner(p *Player, launcherIdx int) bool {
	if launcherIdx < 0 || launcherIdx >= len(r.g.launchers) {
		droppedInputsInc("not_owner")
		return false
	}
	if r.g.launchers[launcherIdx].OwnerPlayerID != p.ID {
		droppedInputsInc("not_owner")
		return false
	}
	return true
}

// checkLauncherMoveRate — 30/sec/player per launcherMoveRateLimit. Mirrors
// checkFireRate's scheme but drops over-quota frames silently (no per-frame
// error emission, so a fast keyboard repeat doesn't spam the client back).
func (r *Room) checkLauncherMoveRate(p *Player) bool {
	now := time.Now().UnixMilli()
	if now-p.launcherMoveWindowStart > 1000 {
		p.launcherMoveWindowStart = now
		p.launcherMoveWindowCount = 0
	}
	if p.launcherMoveWindowCount >= launcherMoveRateLimit {
		droppedInputsInc("rate_limited_launcher_move")
		return false
	}
	p.launcherMoveWindowCount++
	return true
}

// checkFireRate — 30/sec/player. Returns false if exceeded (and emits one
// error frame to the client through the outbound chan).
func (r *Room) checkFireRate(p *Player) bool {
	now := time.Now().UnixMilli()
	if now-p.fireWindowStart > 1000 {
		p.fireWindowStart = now
		p.fireWindowCount = 0
	}
	if p.fireWindowCount >= 30 {
		// emit one-shot error
		errFrame, _ := json.Marshal(map[string]any{
			"type":    "error",
			"code":    "rate_limited",
			"message": "input.fire above 30/sec",
		})
		r.send(p, errFrame)
		droppedInputsInc("rate_limited")
		return false
	}
	p.fireWindowCount++
	return true
}

// send — non-blocking outbound for a single player.
func (r *Room) send(p *Player, frame []byte) {
	if p == nil || p.outbound == nil {
		return
	}
	select {
	case p.outbound <- frame:
	default:
		// writer is slow; drop. Don't disconnect — the writer's own write
		// timeout will close eventually if truly wedged.
	}
}

// broadcast — fan-out a pre-marshaled frame to all connected players.
func (r *Room) broadcast(frame []byte) {
	for _, p := range r.players {
		if p.disconnected {
			continue
		}
		r.send(p, frame)
	}
}

// broadcastRoster — re-marshal + send to everyone.
func (r *Room) broadcastRoster() {
	frame := r.marshalRoster()
	r.broadcast(frame)
}

// broadcastShutdown — server going away; send a 1001-style hint. The ws.go
// writer goroutine takes care of the actual close code.
func (r *Room) broadcastShutdown() {
	frame, _ := json.Marshal(map[string]any{"type": "server.shutdown"})
	r.broadcast(frame)
}

// marshalRoster — schema doc §roster.update.
func (r *Room) marshalRoster() []byte {
	entries := make([]map[string]any, 0, len(r.players))
	for _, p := range r.players {
		entries = append(entries, map[string]any{
			"player_id":  p.ID,
			"name":       p.Name,
			"color":      p.Color,
			"connected":  !p.disconnected,
			"joined_at":  p.JoinedAt,
			"intercepts": p.Intercepts,
			"shots":      p.Shots,
		})
	}
	frame := map[string]any{
		"type":   "roster.update",
		"roster": entries,
	}
	b, _ := json.Marshal(frame)
	return b
}

// marshalSnapshot — schema doc §state.tick.
func (r *Room) marshalSnapshot(tickNum uint32) []byte {
	snap := r.g.snapshotMap()
	snap["type"] = "state.tick"
	snap["tick"] = tickNum
	snap["ts"] = time.Now().UnixMilli()
	b, _ := json.Marshal(snap)
	return b
}

// marshalWaveSummary — schema doc §wave.summary. Emitted once on every
// transition out of COMBAT that yields reviewable kills (COMBAT→WAVE_END +
// COMBAT/WAVE_END→GAME_OVER).
func (r *Room) marshalWaveSummary(wave int, kills []killSnapshot, leaked int, shahedPaths, interceptorPaths []pathRecord) []byte {
	out := make([]map[string]any, 0, len(kills))
	for _, k := range kills {
		path := make([]map[string]float64, 0, len(k.Path))
		for _, pp := range k.Path {
			path = append(path, map[string]float64{"x": pp.X, "y": pp.Y})
		}
		world := make([]map[string]any, 0, len(k.WorldShaheds))
		for _, w := range k.WorldShaheds {
			world = append(world, map[string]any{
				"eid":    w.EID,
				"pos":    map[string]float64{"x": w.X, "y": w.Y},
				"vel":    map[string]float64{"x": w.VX, "y": w.VY},
				"cruise": w.Cruise,
			})
		}
		out = append(out, map[string]any{
			"shahed_eid":    k.ShahedEID,
			"kill_pos":      map[string]float64{"x": k.KillX, "y": k.KillY},
			"player_id":     k.PlayerID,
			"tick":          k.Tick,
			"path":          path,
			"world_shaheds": world,
		})
	}
	// Encode the path arrays — one record per dead shahed and one per dead
	// interceptor, each carrying its trajectory + outcome string. Used by
	// mp.html's drawAllTrajectories to render the green/red/blue/gray
	// color-coded paths at WAVE_END / GAME_OVER (matches solo index.html
	// L1548-L1582 drawTrajectories).
	encodePathRec := func(records []pathRecord) []map[string]any {
		encoded := make([]map[string]any, 0, len(records))
		for _, pr := range records {
			pathArr := make([]map[string]float64, 0, len(pr.Path))
			for _, pp := range pr.Path {
				pathArr = append(pathArr, map[string]float64{"x": pp.X, "y": pp.Y})
			}
			encoded = append(encoded, map[string]any{
				"path":    pathArr,
				"outcome": pr.Outcome,
			})
		}
		return encoded
	}
	frame := map[string]any{
		"type":               "wave.summary",
		"wave":               wave,
		"shaheds_killed":     len(kills),
		"shaheds_leaked":     leaked,
		"kills":              out,
		"shaheds":            encodePathRec(shahedPaths),
		"interceptors":       encodePathRec(interceptorPaths),
	}
	b, _ := json.Marshal(frame)
	return b
}

// sweepDisconnected — remove players whose reconnect window has elapsed.
// The Hub holds the token TTL; we mirror the eviction in the roster by
// asking the Hub. Despawning the player's launcher on eviction is the
// other half of the per-player ownership lifecycle (schema doc §"Rooms and
// session lifecycle").
func (r *Room) sweepDisconnected() {
	now := time.Now()
	var evicted bool
	for id, p := range r.players {
		if !p.disconnected {
			continue
		}
		r.hub.mu.Lock()
		slot, ok := r.hub.reconnectTokens[p.ReconnectToken]
		r.hub.mu.Unlock()
		if !ok || now.After(slot.expiresAt) {
			// Despawn the player's launcher. In-flight interceptors stay
			// alive — they carry OwnerPlayerID and don't depend on the
			// launcher entity.
			r.g.despawnLauncherForOwner(p.ID)
			delete(r.players, id)
			evicted = true
		}
	}
	if evicted {
		r.broadcastRoster()
	}
}

// shouldCelebrate — runs on the tick goroutine. Synchronous DB read against
// the "all-time" top-100, deriving rank by counting strictly-greater scores.
// Top-5 (rank ≤ 5) triggers CELEBRATION. DB latency budget: a single SELECT
// over ≤100 rows behind a 100ms timeout. On any error, default to no-celebrate.
//
// This is the only synchronous DB call from the tick loop; SQLite is fast
// enough that this isn't a real budget concern, but the timeout exists so a
// degraded DB can't lock the loop.
func (r *Room) shouldCelebrate(finalScore int64) bool {
	if r.hub.db == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// MP CELEBRATION gates against the MP-only leaderboard so an SP top-5
	// doesn't accidentally trigger a celebration for an MP run that wouldn't
	// have ranked among other MP runs.
	scores, err := queryTop(ctx, r.hub.db, 0, 100, "mp")
	if err != nil {
		log.Printf("celebrate: queryTop failed: %v", err)
		return false
	}
	rank := 1
	for _, s := range scores {
		if s.Score > finalScore {
			rank++
		}
	}
	return rank <= 5
}

// onGameOver — runs in its own goroutine (off the tick path) so it can
// touch the DB without stalling the tick. Emits the game.over frame +
// auto-submits the leaderboard score.
func (r *Room) onGameOver() {
	// Build ranks. We need a stable snapshot of player contributions; the
	// tick goroutine is the owner, so we ask synchronously via a one-shot
	// chan. Simpler approach: read directly here — Player fields are atomic
	// writes from the tick goroutine and we accept a small race.
	ranks := make([]rankEntry, 0, len(r.players))
	for _, p := range r.players {
		ranks = append(ranks, rankEntry{
			PlayerID:   p.ID,
			Name:       p.Name,
			Intercepts: p.Intercepts,
			Shots:      p.Shots,
			contribute: 10*int(p.Intercepts) - int(p.Shots),
		})
	}
	// Descending sort by contribution (simple bubble — N≤8).
	for i := 0; i < len(ranks); i++ {
		for j := i + 1; j < len(ranks); j++ {
			if ranks[j].contribute > ranks[i].contribute {
				ranks[i], ranks[j] = ranks[j], ranks[i]
			}
		}
	}
	for i := range ranks {
		ranks[i].Rank = i + 1
	}

	// Capture final score + wave from the (immutable-after-game-over) game state.
	finalScore := r.g.score
	finalWave := r.g.wave

	// Build game.over frame.
	overFrame, _ := json.Marshal(map[string]any{
		"type":        "game.over",
		"final_score": finalScore,
		"final_wave":  finalWave,
		"ranks":       ranks,
	})
	r.broadcast(overFrame)

	// Auto-submit to leaderboard. Construct name per schema doc:
	//   - solo: top player's name
	//   - N≥2:  "<top>+<N-1>"
	connected := 0
	for _, p := range r.players {
		if !p.disconnected {
			connected++
		}
	}
	if connected == 0 || len(ranks) == 0 {
		return // no humans saw the game; don't pollute the leaderboard
	}
	topName, _ := normaliseName(ranks[0].Name)
	submitName := topName
	if connected >= 2 {
		submitName = topName + "+" + itoa(connected-1)
		if len(submitName) > 14 {
			submitName = submitName[:14]
		}
	}
	// Build members CSV (highest-contribution first) and auto-derive the
	// team display label. The team string is what the leaderboard panel
	// renders as the entry's primary identifier; for MP it's intentionally
	// the same shape as submitName (and may be wider — team has 24 chars vs
	// name's 14). No user input — last round's team-name prompt was scrapped.
	members := membersCSV(ranks)
	teamLabel := topName
	if connected >= 2 {
		teamLabel = topName + "+" + itoa(connected-1)
		if len(teamLabel) > 24 {
			teamLabel = teamLabel[:24]
		}
	}
	r.autoSubmitScore(submitName, finalScore, finalWave, teamLabel, members)
}

// rankEntryNames — minimal interface so autoSubmitScore can build members CSV
// without taking a dependency on the local rankEntry type. The slice is small
// (≤ maxPlayers = 8) so an inline join is cheap.
func membersCSV(ranks []rankEntry) string {
	parts := make([]string, 0, len(ranks))
	for _, r := range ranks {
		// Skip entirely if the raw name was empty — normaliseName would
		// substitute "ANON", which would muddy the CSV with placeholder
		// rows for un-named contributors.
		if strings.TrimSpace(r.Name) == "" {
			continue
		}
		n, _ := normaliseName(r.Name)
		if n == "" {
			continue
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, ",")
}

// rankEntry — same shape as the anonymous struct in onGameOver, hoisted so
// membersCSV can take it as a typed parameter (and so tests can build one
// without round-tripping through onGameOver).
type rankEntry struct {
	PlayerID   uint32 `json:"player_id"`
	Name       string `json:"name"`
	Intercepts uint32 `json:"intercepts"`
	Shots      uint32 `json:"shots"`
	Rank       int    `json:"rank"`
	contribute int
}

// autoSubmitScore — bypasses the rate limiter (server is the submitter,
// not the client). Mirrors the insert + prune + SSE-broadcast from
// handleSubmit() in main.go. Errors are logged but not surfaced — the
// game-over UX shouldn't depend on a leaderboard insert.
//
// Writes mode='mp' so the row is filtered to MP-only leaderboard queries
// (single-player POSTs land on mode='sp'). Team is the auto-derived label
// (e.g. "ALICE+2"); members is the comma-joined contributor list.
func (r *Room) autoSubmitScore(name string, score int64, wave int, team, members string) {
	if r.hub.db == nil {
		return
	}
	if err := validateSubmission(Submission{Score: score, Wave: wave}); err != nil {
		log.Printf("mp auto-submit: validation failed: %v", err)
		return
	}
	now := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := r.hub.db.ExecContext(ctx,
		`INSERT INTO scores (name, score, wave, created_at, team, members, mode) VALUES (?, ?, ?, ?, ?, ?, 'mp')`,
		name, score, wave, now, team, members)
	if err != nil {
		log.Printf("mp auto-submit: insert: %v", err)
		return
	}
	id, _ := res.LastInsertId()
	if _, err := r.hub.db.ExecContext(ctx,
		`DELETE FROM scores WHERE id NOT IN (
			SELECT id FROM scores ORDER BY score DESC, created_at ASC LIMIT 100
		)`); err != nil {
		log.Printf("mp auto-submit: prune: %v", err)
	}
	entry := Score{ID: id, Name: name, Score: score, Wave: wave, CreatedAt: now, Team: team, Members: members, Mode: "mp"}
	if payload, err := json.Marshal(entry); err == nil && r.hub.sseHub != nil {
		r.hub.sseHub.broadcast(payload)
	}
	log.Printf("mp auto-submit: %q score=%d wave=%d team=%q members=%q", name, score, wave, team, members)
}

// itoa — tiny non-allocating wrapper; we only call with small positive ints (N-1).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	// fast path for 1..9 covers all realistic N-1 cases (N up to maxPlayers=8)
	if n > 0 && n < 10 {
		return string(rune('0' + n))
	}
	// fallback for any larger value
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ─── Metric counters (package-global; rendered in main.go) ───
// The Zenoh REST publisher in zenoh.go writes to mpZenohPubErrs and
// mpZenohPubByType (keyed by event_type) on every publish attempt /
// success. When ZENOH_PUBLISH=0 (default) the publisher goroutine never
// starts and these counters stay at zero — metrics render the same shape
// either way. (Prior to the REST pivot, these were linked in
// both build-tag modes because of a //go:build !zenoh stub; the stub is
// gone now that the publisher is pure stdlib and always linked.)
var (
	droppedInputs    sync.Map // map[string]*atomic.Int64
	tickDurMicro     atomic.Int64
	tickCount        atomic.Int64
	mpZenohPubErrs   atomic.Int64
	mpZenohPubByType sync.Map // map[string]*atomic.Int64, keyed by event_type
)

func mpZenohPubByTypeInc(t string) {
	v, ok := mpZenohPubByType.Load(t)
	if !ok {
		nv := &atomic.Int64{}
		actual, loaded := mpZenohPubByType.LoadOrStore(t, nv)
		if loaded {
			v = actual
		} else {
			v = nv
		}
	}
	v.(*atomic.Int64).Add(1)
}

func mpZenohPubByTypeSnapshot() map[string]int64 {
	out := map[string]int64{}
	mpZenohPubByType.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

func droppedInputsInc(reason string) {
	v, ok := droppedInputs.Load(reason)
	if !ok {
		nv := &atomic.Int64{}
		actual, loaded := droppedInputs.LoadOrStore(reason, nv)
		if loaded {
			v = actual
		} else {
			v = nv
		}
	}
	v.(*atomic.Int64).Add(1)
}

func droppedInputsSnapshot() map[string]int64 {
	out := map[string]int64{}
	droppedInputs.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}
