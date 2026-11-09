# Air Defenders — Co-op Multiplayer

## Conclusion

Multiplayer is added to the existing self-contained Go service ([ops/serve/airdefenders/main.go](../../ops/serve/airdefenders/main.go)) as one new endpoint, `GET /api/mp/ws`, and a server-authoritative game loop running per **room**. Every connected browser is a thin renderer: it sends input verbs and renders 30-Hz full-state snapshots, interpolating to 60 fps client-side. Single-player is unchanged in feel — it is just a 1-player room. Lives, score, and wave are shared. The four engagement-relevant moments emit on an in-process `EventBus` consumed by (a) future Zenoh publisher (Agent 3) and (b) the SSE leaderboard hub on game-over. The Zenoh path is feature-flagged off and additive; the existing endpoints and SQLite leaderboard are untouched.

## Topology

```text
                browser N (one per player)
                 ▲              │
                 │ WS frames    │ input verbs
                 │ (snapshots)  ▼
            ┌────────────────────────────┐
            │  WS hub  (/api/mp/ws)      │  ─── per-room goroutine
            │  - one Room per match      │       30 Hz tick → snapshot fan-out
            └──────────────┬─────────────┘
                           │ Publish(GameEvent)        non-blocking
                           ▼
                  ┌────────────────┐
                  │   EventBus     │  in-process, buffered chans, drops on overflow
                  └───┬────────┬───┘
                      │        │
                      ▼        ▼
        SSE leaderboard   Zenoh publisher (Agent 3)
        (game.over only)  airdefenders/<site>/<event>/v1
                          (feature-flagged: ZENOH_PUBLISH=1)
```

The Zenoh publisher is a **subscriber** to the EventBus, not in the game loop's critical path. The tick goroutine calls `EventBus.Publish` and returns immediately; if a subscriber is slow, its event is dropped (`goat_audit_write_errors_total`-style metric).

## File layout

Single Go package; two embedded HTML pages.

```text
ops/serve/airdefenders/
├── main.go              — wiring + leaderboard; routes / and /mp
├── index.html           — verbatim pre-multiplayer single-player baseline
│                          (2656 lines) + small CO-OP MULTIPLAYER → button
│                          on the title screen that navigates to /mp
├── mp.html              — WS-driven co-op renderer; sends input verbs to
│                          /api/mp/ws and renders interpolated snapshots
├── multiplayer.go       — Hub, Room, tick loop, snapshot, input dispatch,
│                          per-player launcher ownership + reserved slots
├── ws.go                — WS handshake + read/write loop, ping/pong
├── eventbus.go          — EventBus interface + in-process impl
├── game.go              — server-authoritative physics
└── zenoh.go             — Zenoh REST publisher (HTTP PUT to the
                            rest-pub-bridge mTLS sidecar; no build tag,
                            pure stdlib)
```

Dependencies: `nhooyr.io/websocket` only (Zenoh publisher uses stdlib `net/http`).

## WebSocket endpoint

| Field | Value |
| --- | --- |
| Path | `GET /api/mp/ws` |
| Subprotocol | none |
| Origin policy | accept any origin **only when** `Host` matches `airdefenders.*` or `Origin` is empty; rationale: service is mesh-internal (`ipallowlist 100.64.0.0/10` at Traefik). Behind that, browser origin checks add no security; permissive here keeps the same posture as `/api/scores`. |
| Compression | disabled (`CompressionMode: Disabled`) — snapshots are small JSON, latency over CPU |
| Max message size | 16 KiB (server reads). Inbound input frames are <200 B; the read limit is set above outbound state.tick worst-case so the same constant can be used for both sides. Outbound state.tick frames on a busy COMBAT tick hit ~6-8 KiB (per-entity `path[]` history × ~10 entities + camels/spits + bunker block); 16 KiB leaves headroom. |
| Server → client ping cadence | every 10 s; client must `pong` within 15 s |
| Server-side write timeout | 5 s per frame |
| Server-side read deadline | reset on every frame; idle 25 s → close |
| Reconnect | client may reconnect with a session token (see §Lifecycle) to retain `player_id` |
| Per-IP cap | `defaultMaxConnsPerIP = 8` concurrent WS connections per IP (separate from leaderboard rate limiter), overridable via `AD_MAX_CONNS_PER_IP`. 8 = `maxPlayers`, so a single household/NAT can field a full co-op room, and the auto-pilot bot fleet (§Auto-pilot personae) spawns up to 8 browser windows from one laptop IP. Raise it to also join as a human; lower it to harden a public deployment. |
| Per-IP input rate-limit | `input.fire` capped at 30/sec/player (mirrors existing `fireFromLauncher` 15-frame `spaceCooldown` at 60 fps = ~4/sec sustained; 30/sec headroom for burst) |

Close codes: `4000` invalid hello, `4001` reconnect token unknown, `4002` rate-limited, `4003` server full, `1000` normal, `1001` going away (server shutdown).

## Tick model

- **Server tick rate: 30 Hz** (33.3 ms / tick). Justification: existing browser loop is `requestAnimationFrame` at ~60 Hz; physics constants (e.g. `SWEEP_RATE`, `ARM_TIME=60` frames = 1 s, `RESUPPLY_TIME=120` = 2 s) assume 60 Hz. 30 Hz halves bandwidth vs 60 Hz while preserving sub-frame precision on the client via interpolation. All per-frame timers in the existing code (`armTimer`, `resupplyTimer`, `waveTimer`, `bunkerTimer`, `bunkerRunTimer`, `bunkerPostTimer`, `spaceCooldown`, `lifeUpFlash`, `killFlash`, `camelSpitCooldown`) are reinterpreted as **tick-count timers at 30 Hz** in the server, with their numeric constants halved when ported (e.g. `ARM_TIME=30`, `RESUPPLY_TIME=60`). Agent 2's browser renders the latest snapshot at 60 fps via linear interpolation between consecutive snapshots. The browser paints once per display refresh at any rate; the radar sweep advances by elapsed time, not per paint. Single-player (`index.html`) is built: it keeps its per-frame constants and runs its game step on a fixed 60 Hz timestep driven by elapsed time, so game speed no longer follows the monitor's refresh rate.
- **Snapshot policy: full snapshots only in v1.** Per-tick payload is ~1-3 KiB JSON for a typical wave; at 30 Hz that's ~30-90 KiB/s per client. Acceptable for mesh-internal use. Delta-snapshot encoding is flagged in [§Out of scope](#out-of-scope) as future work.
- **Input application:** inputs arrive asynchronously and are pushed into the room's per-player `inputQueue` ([]InputEvent). At the top of each tick the room drains all queues in `(received-timestamp ASC)` order, applies each verb, then advances physics. Inputs received during a tick land in the next tick.
- **Authoritative state lives only on the server.** Clients render the latest snapshot they have received and never simulate physics locally. The browser keeps zero authoritative game state.

## Rooms and session lifecycle

- **One global room in v1.** Up to 8 connected players share one game. On `welcome` the server tells the client `room_id: "global"` to leave the door open for per-site / private rooms post-v1.
- **player_id assignment.** First WS frame must be `hello`. Server allocates `player_id` = monotonic uint32 starting at 1. Each player gets a deterministic display color from a fixed 8-entry palette (`player_id % 8`).
- **Reconnect token.** `welcome` includes `reconnect_token` = opaque 128-bit hex string. Server retains `(player_id, reconnect_token)` for **30 seconds** after a player's WS closes for any reason. A new WS `hello` with `reconnect_token` set, received within that window, restores the same `player_id`, color, and contribution counters. After 30 s the slot is freed.
- **Disconnect.** Immediate WS close → player marked `disconnected=true` in the roster; server broadcasts `roster.update`. After 30 s without reconnect, the player is removed and a second `roster.update` is broadcast.
- **Launcher spawn/despawn lifecycle.** A fresh `hello` (no reconnect token, or expired one) triggers a launcher spawn at the first free reserved-X-slot; the new launcher carries `owner_player_id = player_id` with full ammo and `fire_mode=0`. A reconnecting `hello` (valid token within the 30 s window) does NOT spawn — the launcher is still in the snapshot because immediate-disconnect leaves the launcher entity untouched; only slot-expiry (via `sweepDisconnected`) removes it. In-flight interceptors fired by a player who has since despawned stay alive until they detonate or expire — they carry their own `owner_player_id` and don't depend on the launcher entity's continued existence.
- **Launchers persist across game resets.** A `GAME_OVER → TITLE` transition (via `input.state_advance` ⇒ `gameState.reset()`) does NOT remove launchers — they belong to Players, not Games, and persist as long as their owner stays connected. The reset clears in-flight `arming` / `resupplying` timers and refills `interceptors_left`; `owner_player_id`, `slot`, `x`, and `fire_mode` survive untouched. The only paths that despawn a launcher are `sweepDisconnected` (after 30 s reconnect-window expiry) and the 5-min idle-room reset (which only runs when no players are connected). This is the same persistence shape as `WAVE_END → EMPLACEMENT`: a player's deliberate aim (X), mode choice (fire_mode), and identity (owner, slot) outlive any single game.
- **0 players connected → game pauses.** Wave timer, shahed spawns, bunker schedule, and physics integration freeze. `lives`, `score`, `wave`, `state` are preserved. First reconnect within the pause resumes. After **5 minutes idle with zero players**, the room resets to `TITLE` state with `score=0, lives=5, wave=0` so a stale game doesn't haunt the first new joiner.
- **CELEBRATION redirect.** When the room transitions `phaseCombat → phaseGameOver`, the server queries the all-time top-100 leaderboard (synchronous SQLite SELECT, 100 ms timeout); if the room's final score lands at rank ≤ 5, the phase is redirected into `phaseCelebration` for **60 ticks (= 2 s at 30 Hz)** before flipping to `phaseGameOver`. During CELEBRATION the snapshot keeps re-broadcasting the same frozen physics body with `phase: "CELEBRATION"`; the client renders confetti based on the phase flag (no server-side particle system). Auto-submit + the `game.over` frame fire on the eventual GAME_OVER transition, not on CELEBRATION entry — so the leaderboard insert is unaffected.

## WebSocket message schema

All frames are JSON text frames. Every message has `type` (string) plus message-specific fields. Unknown fields are ignored; missing required fields → close 4000.

**Common types.** `Vec2` = `{"x":number,"y":number}` in 800×700 world space (floats). `eid` = server-assigned monotonic uint32, stable for the entity's lifetime. `tick` = uint32 server tick counter. `ts` = int64 unix millis, derived from server `time.Now()` at tick start.

### Client → Server

| `type` | Fields | Maps to / behaviour |
| --- | --- | --- |
| `hello` (first frame, required) | `name` string opt 1-14 chars normalised same as leaderboard; `reconnect_token` string opt 128-bit hex | Server replies `welcome` or closes 4001/4000 |
| `input.fire` | `launcher_idx` int **advisory**; `aim` Vec2 opt — if present, server steers resulting interceptor toward this point | **Owner-only.** Server resolves to the sender's owned launcher_idx (each player owns exactly one launcher); any client-supplied `launcher_idx` is ignored. If the sender owns no launcher, drop with `dropped_inputs{reason="not_owner"}`. `fireFromLauncher(owned_idx)`. `spaceCooldown` (15 ticks = 0.5 s) enforced **per player** — players fire in parallel |
| `input.launcher_switch` | `launcher_idx` int (ignored) | **Legacy no-op.** Per-player ownership means each player owns exactly one launcher; there is nothing to switch. Kept on the dispatcher whitelist so older clients don't get `malformed` errors during the change-set roll-forward. |
| `input.launcher_move` | `launcher_idx` int required; `dx` float required ∈ `[-1.5, 1.5]` (clamped server-side) | **Owner-only.** Server checks `launchers[idx].owner_player_id == sender.player_id`; on mismatch drop with `dropped_inputs{reason="not_owner"}`. On pass, apply `launchers[idx].x += dx` then clamp to `[40, GAME_W-40]`. Per-player rate-limit 30/sec (drops over-quota frames; `dropped_inputs{reason="rate_limited_launcher_move"}`). Unknown `launcher_idx` drops with `dropped_inputs{reason="not_owner"}` (out-of-bounds is treated as not-owned). |
| `input.fire_mode` | `launcher_idx` int required; `mode` int required ∈ {0=manual, 1=armed-proximity, 2=autopilot} | **Owner-only.** Per-player launcher ownership — each player controls their own launcher's mode independently. Server checks ownership; mismatch drops with `dropped_inputs{reason="not_owner"}`. |
| `input.resupply` | `launcher_idx` int required | **Owner-only.** `triggerResupply(launcher_idx)`. Server checks ownership; mismatch drops with `dropped_inputs{reason="not_owner"}`. |
| `input.steer` | `interceptor_eid` uint32 required; `aim` Vec2 required | Server lerps target interceptor's `vx,vy` toward aim. Owner-only — non-owner ignored silently. Client tracks ownership via `welcome.player_id` ↔ `interceptors[i].owner_player_id` |
| `input.bunker_run` | none | `startBunkerRun()`. Any player; first within warning window wins |
| `input.camel_release` | none | `releaseCamel()`. Pool shared |
| `input.radar_place` | `radar_idx` int required; `pos` Vec2 required | EMPLACEMENT only. Server clamps to `(40..760, 100..580)`. Last-writer-wins per tick |
| `input.state_advance` | none | Space-key state transitions: `TITLE→EMPLACEMENT`, `EMPLACEMENT→COMBAT`, `WAVE_END→EMPLACEMENT` (only after `waveEndTimer > 60` ticks = 2 s), `GAME_OVER→TITLE`. Any player. `spaceCooldown` enforced |
| `pong` | `echo` string required (server-ping nonce) | Liveness reply |

### Server → Client

#### `welcome` (sent once, immediately after `hello`)

```jsonc
{
  "type": "welcome",
  "player_id": 3,
  "reconnect_token": "ab12...",  // 128-bit hex; client MUST persist in sessionStorage
  "room_id": "global",
  "server_tick_hz": 30,
  "world": { "w": 800, "h": 700 },
  "color": "#00ff88",            // RGB hex; deterministic from player_id
  "snapshot": { /* full state.tick payload — see below */ },
  "roster": [ /* same shape as roster.update.roster */ ]
}
```

#### `state.tick` (sent every server tick)

```jsonc
{
  "type": "state.tick",
  "tick": 4242,
  "ts": 1747654321000,
  "phase": "COMBAT",             // TITLE | EMPLACEMENT | COMBAT | WAVE_END | CELEBRATION | GAME_OVER
  "wave": 7,
  "lives": 5,
  "score": 8420,
  "intercepts": 17,              // for life-up display
  "life_up_threshold": 24,
  "wave_timer": 1830,            // ticks since wave start
  "wave_end_timer": 0,
  "space_cooldown": 0,           // global; per-player cooldown is implicit in input rate limiting
  "shaheds": [
    {
      "eid": 1234,
      "pos": { "x": 412.7, "y": 188.3 },
      "vel": { "x": 0.6, "y": 1.4 },
      "alive": true,
      "cruise": false,
      "pattern": 3,
      "visible": 47,              // 0 when outside any radar coverage; >0 when tracked
      "path": [                   // last 30 (x,y) positions during COMBAT; absent otherwise
        { "x": 410.1, "y": 180.4 },
        { "x": 411.0, "y": 184.2 }
        // ... (up to 30 entries)
      ]
    }
  ],
  "interceptors": [
    {
      "eid": 5678,
      "pos": { "x": 400.0, "y": 540.2 },
      "vel": { "x": 0.1, "y": -3.4 },
      "alive": true,
      "armed": true,
      "auto_pilot": false,
      "age": 12,
      "owner_player_id": 3,
      "path": [                   // last 30 (x,y) positions during COMBAT; absent otherwise
        { "x": 400.0, "y": 580.0 }
        // ... (up to 30 entries)
      ]
    }
  ],
  "launchers": [
    {
      "idx": 0,
      "x": 400,
      "interceptors_left": 7,
      "fire_mode": 1,
      "arming": false,
      "arm_timer": 0,
      "resupplying": false,
      "resupply_timer": 0,
      "owner_player_id": 3
    }
  ],
  "radars": [
    { "idx": 0, "pos": { "x": 200, "y": 300 }, "radius": 120 }
  ],
  "bunker": {                    // null when bunkerPhase == 'idle'
    "phase": "active",           // "active" | "post"
    "timer": 210,                // ticks-remaining at server's 30 Hz (active: warning ticks; post: post-phase ticks). To convert to seconds for HUD: timer / 30.
    "run_timer": 0,              // 0 until RUN pressed, then ticks since (also at 30 Hz)
    "outcome": "pending"         // "pending" | "safe" | "hit"
  },
  "camel_mode": false,
  "camel_pool": 0,
  "camels": [                    // present on every snapshot; empty outside CAMEL_WAVE
    {
      "eid": 9001,
      "pos": { "x": 220.0, "y": 650.0 },  // y is fixed at ground line (worldH-50)
      "dir": 1,                  // 1 = walking right, -1 = walking left
      "walking": true,
      "alive": true
    }
  ],
  "camel_spits": [               // ephemeral projectiles fired by camels
    {
      "eid": 9101,
      "pos": { "x": 240.0, "y": 600.0 },
      "vel": { "x": 1.5, "y": -2.0 },
      "age": 8,
      "alive": true
    }
  ],
  "explosions": [                // ephemeral; clients render then forget
    { "x": 410, "y": 190, "color": "#ff8800", "age": 0 }
  ],
  "blast_waves": [
    { "x": 410, "y": 190, "radius": 60, "max_radius": 147 }
  ],
  "kill_flash": 0,
  "life_up_flash": 0
}
```

Notes for the renderer (Agent 2):
- Coordinates are floats; the existing single-player code uses floats internally and ints at draw time. Snapshot floats land in the same canvas.
- `eid` is stable across ticks; use it as a React-key-style identity for interpolation between snapshots.
- An entity disappearing from a snapshot = "removed by server"; do not animate its death — the matching `explosion` / `blast_wave` is the visual.
- `phase` is the renderer's only state machine input. Do not infer state from other fields.

#### `roster.update` (broadcast on join, leave, reconnect, disconnect)

```jsonc
{
  "type": "roster.update",
  "roster": [
    {
      "player_id": 1,
      "name": "ALICE",
      "color": "#00ff88",
      "connected": true,
      "joined_at": 1747654300000,
      "intercepts": 11,           // contribution counter, this session
      "shots": 23
    }
  ]
}
```

#### `game.over` (sent once when the room transitions to `GAME_OVER`)

```jsonc
{
  "type": "game.over",
  "final_score": 8420,
  "final_wave": 8,
  "ranks": [                     // descending by contribution_score = 10*intercepts - shots
    { "player_id": 3, "name": "BOB",   "intercepts": 14, "shots": 22, "rank": 1 },
    { "player_id": 1, "name": "ALICE", "intercepts": 11, "shots": 23, "rank": 2 }
  ]
}
```

The room continues to emit `state.tick` while in `GAME_OVER` (so latecomers see the screen). Submission to the existing leaderboard is **automatic, server-side**, using the room-wide score and the top-ranked player's display name with `+<N>` suffix when N≥2 connected (e.g. `"BOB+2"`). This keeps the existing leaderboard semantically meaningful without needing per-player score rows.

#### `wave.summary` (broadcast once per COMBAT-out transition)

Emitted once on `phaseCombat → phaseWaveEnd` AND once on `phaseCombat → phaseGameOver` (whether via lives→0 or via bunker hit). Carries per-kill snapshots so the client can render the WAVE_END trajectory-review UX (a "click a kill marker for the freeze-frame trajectory" affordance). Distinct from `state.tick` so the recurring snapshot stays small.

```jsonc
{
  "type": "wave.summary",
  "wave": 7,
  "shaheds_killed": 14,        // == kills.length
  "shaheds_leaked": 2,         // count of leakers that lost lives this wave
  "kills": [
    {
      "shahed_eid": 1234,
      "kill_pos": { "x": 410.0, "y": 190.0 },
      "player_id": 3,          // owner of the interceptor that scored the kill; 0 = system (armed-mode launched without owner)
      "tick": 412,              // server tick (relative to wave) at which the kill happened
      "path": [                  // killed shahed's trajectory, last 30 samples up to and including kill_pos
        { "x": 380.2, "y": 60.3 },
        { "x": 384.8, "y": 64.9 }
        // ... up to 30 entries (one second of wall-clock at 30 Hz)
      ]
    }
  ]
}
```

Path commitment: 30 entries — one second of trajectory leading up to the kill, sampled every other tick. Matches the per-entity `state.tick.shaheds[].path` ring buffer length. Empty `kills` is suppressed (no `wave.summary` frame is sent if both `kills` is empty AND `shaheds_leaked` is zero).

#### `ping`

```jsonc
{ "type": "ping", "nonce": "abc123" }
```

#### `error`

```jsonc
{ "type": "error", "code": "rate_limited", "message": "input.fire above 30/sec" }
```

Non-fatal; connection stays open. Fatal errors close with the relevant 4xxx code.

## Shared state model

| Field | Shared? | Notes |
| --- | --- | --- |
| `lives` | yes | one health pool for the room |
| `score` | yes | sum of all intercepts × wave-weight, same scoring formula as single-player |
| `wave` | yes | progression is shared; everyone advances together |
| `launchers` (count, fire_mode, resupply state, interceptors_left, x) | **per-player ownership** | one launcher per connected player; each launcher carries `owner_player_id`; mode/resupply/move/fire are owner-only |
| `radars` (positions) | yes | shared placement during EMPLACEMENT |
| `interceptors` | per-player ownership for steering | anyone can see all interceptors; only owner steers (§input.steer) |
| `intercepts` (per-player, contribution counter) | per-player | for ranking and life-up attribution |

Each player owns exactly one launcher; ownership conferred at join, freed at disconnect-timeout, preserved across reconnects. Launchers spawn at one of 8 reserved X positions; first free slot wins. Slot X positions are evenly distributed across `[120, GAME_W-120]` at indices 0-7 → `120, 200, 280, 360, 440, 520, 600, 680` px (with `GAME_W=800`). The slot a player holds is preserved across the 30 s reconnect window so a returning player's launcher does not visually migrate; on slot-expiry the launcher is removed from the snapshot and the slot becomes available for the next fresh joiner.

**SOLO MP vs SOLO mode.** A 1-player multiplayer room runs with exactly 1 launcher (per the per-player rule). This is tighter than the original single-player game, which historically gained a second launcher at wave 4. The "single-player feel preserved" promise (below) applies to the **SOLO mode** entry point (no networking; the canonical single-player loop, restored in the same change-set by Agent 2's title-screen mode picker) — not to a 1-player MP room.

**One launcher per player is design-of-record (locked 2026-05-20).** The alternative — extra launchers at higher waves (mirroring solo's wave-4 unlock for each player, or a single per-room unlock that gives every connected player a second launcher) — was considered and rejected. Rejection reasons: (1) the spawn-slot allocator's 8 reserved X positions across `[120, GAME_W-120]` are already tight at 80 px per launcher; doubling to 16 launchers crowds the 50-px chassis to overlap; (2) the difficulty-relief lever solo lacks but MP has natively is *another player joining*, not a second launcher per existing player — the multiplayer game already amortizes the "more threats" pressure across more humans by construction; (3) ownership stays unambiguous as "your launcher" with no per-player active-launcher tracking layer. If a future round revisits this, the cleanest add would be a server-side wave-N unlock that gives every connected player a second launcher; the spawn-slot allocator would need to extend from 8 to 16 reserved positions, and the per-player roster slot would carry a `launcher_idxs[]` instead of a single `launcher_slot`. Out of scope for v1.

Single-player feel preserved (SOLO mode): with the SOLO entry point, behaviour is identical to today's game (the player owns every interceptor; their `intercepts` drives life-up; second launcher at wave 4 still applies because SOLO does not run the multiplayer ownership model).

## Event bus interface

Agent 3 subscribes to this interface; Agent 1 implements it. The full definitive Go signature:

```go
package airdefenders

import "time"

type GameEvent struct {
    Type      string                 // see EventType constants
    Timestamp time.Time              // server time.Now() at emission
    EventID   string                 // UUIDv4, server-minted
    Payload   map[string]any         // shape per Type — see below
}

const (
    EventRadarDetection   = "radar.detection"
    EventMissileFired     = "missile.fired"
    EventMissileIntercept = "missile.intercept"
    EventShahedImpact     = "shahed.impact"
)

type EventBus interface {
    // Subscribe returns a receive-only channel buffered to `buffer` events,
    // plus an unsubscribe function. Calling unsubscribe drains and closes the chan.
    Subscribe(buffer int) (<-chan GameEvent, func())

    // Publish is non-blocking. If any subscriber's channel is full, the event is
    // DROPPED for that subscriber (a metric counter is incremented). The caller
    // (game-loop tick) must never block on a slow subscriber.
    Publish(GameEvent)
}
```

### Per-event payload shapes

#### `radar.detection`

Emitted on the tick when a shahed transitions from `visible == 0` → `visible > 0` (first frame any radar tracks it).

```jsonc
{
  "shahed_eid": 1234,
  "shahed_pos": { "x": 412.7, "y": 188.3 },
  "shahed_vel": { "x": 0.6, "y": 1.4 },
  "shahed_cruise": false,         // true = high-speed cruise variant
  "radar_idx": 1,
  "radar_pos": { "x": 200, "y": 300 },
  "wave": 7,
  "tick": 4242
}
```

#### `missile.fired`

Emitted on the tick when an interceptor is created (post-arming for armed/autopilot modes; immediate for manual).

```jsonc
{
  "interceptor_eid": 5678,
  "launcher_idx": 0,
  "launcher_pos": { "x": 400, "y": 640 },
  "player_id": 3,
  "fire_mode": 1,                 // 0=manual, 1=armed-proximity, 2=autopilot
  "wave": 7,
  "tick": 4242
}
```

#### `missile.intercept`

Emitted on the tick when an interceptor's proximity fuse triggers a kill (shahed within `PROX_RADIUS` of explosion). One event per shahed killed; if one blast kills two shaheds, two events fire.

```jsonc
{
  "interceptor_eid": 5678,
  "shahed_eid": 1234,
  "kill_pos": { "x": 410, "y": 190 },
  "player_id": 3,                 // owner of the interceptor that triggered the kill
  "wave": 7,
  "tick": 4242
}
```

#### `shahed.impact`

Emitted on the tick when a shahed reaches `y >= GAME_H` (ground impact, life lost) — i.e. a leaker.

```jsonc
{
  "shahed_eid": 1234,
  "impact_pos": { "x": 412.7, "y": 700 },
  "lives_remaining": 4,
  "wave": 7,
  "tick": 4242
}
```

The publisher contract (non-blocking) means Agent 3's subscriber can safely do synchronous Zenoh-publish; if Zenoh router is slow, events drop at the channel boundary, not in the game loop.

## Zenoh mapping

Each `GameEvent.Type` maps to one Zenoh key-expression:

| EventType | Zenoh key-expression |
| --- | --- |
| `radar.detection`   | `airdefenders/<site>/radar/detection/v1` |
| `missile.fired`     | `airdefenders/<site>/missile/fired/v1` |
| `missile.intercept` | `airdefenders/<site>/missile/intercept/v1` |
| `shahed.impact`     | `airdefenders/<site>/shahed/impact/v1` |

`<site>` = environment variable `SITE_ID` (default `nethost`). Ansible sets it from the goatnet's `site_id` fact (`airdefenders_site_id: "{{ site_id }}"`), never from a literal.

**Vendor-namespace note.** `airdefenders/` is a **vendor namespace** under the trial-fabric model — not one of the 12 ADR 0844 reserved top-level prefixes (`data/`, `services/`, `health/`, `config/`, `cmd/`, `attest/`, `ops/`, `agents/`, `release/`, `task/`, `ack/`, `telemetry/`). The vendor identity `airdefenders` is the real-app vendor (not a synthetic-data "esoteric animal" — that naming convention is reserved for synthetic-data vendors per the [Block 82R synth-vendor pattern](../design/zenoh-trial-fabric-synth-vendors.md)). The bootstrap script ([`ops/zenoh-trial-peers/bootstrap-airdefenders-portal.sh`](../../ops/zenoh-trial-peers/bootstrap-airdefenders-portal.sh)) registers the vendor + topics in the portal ([ops/goat-zenoh-portal/](../../ops/goat-zenoh-portal/)) and mints an X.509 leaf cert under the goatnet's offline CA per [`docs/design/nova-x509-identity.md`](nova-x509-identity.md). When goat-net production lands its multi-tenant portal, the vendor classification will be revisited; trial-fabric expedient applies.

### Wire-publish path — HTTP PUT to the rest-pub-bridge mTLS sidecar

The Go server publishes with one stdlib **HTTP PUT** per event. It cannot open a native Zenoh client session: the original design called for `github.com/eclipse-zenoh/zenoh-go` with mTLS (mirroring every other Zenoh peer in this repo — synth-vendor, audit-subscriber, panoscope, portal), but **at the time (early 2026)** that module did not resolve on the Go proxy ("unknown revision v0.11.0") and the Eclipse org's maintained Zenoh 1.x bindings were zenoh-c / zenoh-pico / zenoh-python / zenoh-kotlin — no maintained Go client. *(Update: an official cgo `zenoh-go` shipped in Zenoh 1.9.0, 2026-04; the sidecar is retained here because it keeps the game's CGO-free static image and needs no code change — see [`zenoh-rest-pub-bridge.md`](zenoh-rest-pub-bridge.md).)*

The PUT no longer goes to the router's REST plugin directly. It goes to the **rest-pub-bridge sidecar** ([`docs/design/zenoh-rest-pub-bridge.md`](zenoh-rest-pub-bridge.md), [ADR 1023](../adr/1023-zenoh-rest-pub-bridge-for-non-rust-publishers.md)), which mimics the REST plugin's PUT contract on the front and re-publishes each sample over a **native mTLS Zenoh session** carrying the airdefenders leaf:

```
PUT http://airdefenders-zenoh-sidecar:8000/airdefenders/<site>/<event>/v1
Content-Type: application/json
<envelope JSON body>
        │
        └─► sidecar re-puts over mTLS as CN=airdefenders-trial-peer → router
```

**The sidecar restores the wire identity the game can't present itself.** Before it, the game PUT straight to the router's REST plugin — a listener with no client-cert requirement — and was implicitly trusted by sharing the router's docker network. The [ADR 1020](../adr/1020-zenoh-federated-peering-trust-model.md) ingress/egress ACL split removed that blanket trust: publish is now granted only to identified per-vendor subjects, so the identity-less direct-REST path is dropped at ingress (silently — F-177) and the game's data stops appearing in panoscope. The sidecar holds the per-vendor leaf and presents `CN=airdefenders-trial-peer` on a real session, so the router's `access_control` admits the write to `airdefenders/**`. The game needs zero code change — only its `ZENOH_REST_ENDPOINT` repoints from the router to the sidecar. The `airdefenders` vendor must also be in the goatnet's seed-vendor list (`group_vars/<group>/synth-vendors.yml`, key `nova_seed_vendors` — first-instance inheritance) so the router renders the matching subject + rule + storage. The eventual goat-net production target — a native maintained-language (Rust/Python) publisher — supersedes both the REST shim and the sidecar; the sidecar serves the trial fabric and any transition.

**Per-vendor X.509 + portal topic registration STILL APPLY.** The portal inspector's "decoded samples" path and the schema viewer / autogen-code surfaces use the uploaded `.fds` descriptors and the registered topic rows — independent of the wire-publish auth surface. See §".proto schemas" below; the bootstrap script unchanged in posture.

The publisher is pure stdlib (`net/http` + `encoding/json`) so the Go binary stays CGO-free and the static distroless runtime image is preserved unconditionally — no build-tag gating, no glibc-dynamic variant. Whether the publisher actually emits is a runtime decision: `ZENOH_PUBLISH=1` env var on. A goatnet that leaves `airdefenders_zenoh_enabled` at the role default (false) leaves the env var unset and runs the byte-identical binary with no publisher.

### Envelope

Every Zenoh payload is a JSON object:

```jsonc
{
  "envelope": {
    "site_id": "nethost",
    "event_id": "1b67c0e0-1d2b-4a8e-90c1-9f4e23a8b7e2", // UUIDv4
    "event_type": "radar.detection",
    "ts_server": 1747654321000,                          // unix millis
    "ts_iso": "2026-05-19T12:34:56.000Z",                // RFC3339Nano
    "schema_version": "v1",
    "source": {
      "service": "airdefenders",
      "instance": "<HOSTNAME env>",
      "room_id": "global"
    }
    // NTP attestation in the envelope is deliberately deferred to Block 11 Step 9.
    // Step 9 introduces a reusable `telemetry-client` Go module that all fabric
    // publishers will import for NTP-attested timestamps. Wiring `chronyc` (or
    // equivalent) into airdefenders here would build the exact "airdefenders-only
    // code" Step 9 was scoped to prevent. Re-enable when Step 9 lands.
  },
  "payload": { /* per-event-type shape from §Event bus interface */ }
}
```

Aligns with the planned Block 11 Step 9 envelope shape ([`docs/design/airdefenders-leaderboard.md` §Future](airdefenders-leaderboard.md#future--planned-in-block-11-step-9)). NTP attestation in the envelope is the one remaining piece — explicitly deferred to Block 11 Step 9 per the comment above. Schema-registry binding (the `.proto` sources + portal-upload + `format=protobuf:<MsgName>` flip) **has landed** — see §".proto schemas" below.

### .proto schemas

Canonical `.proto` sources for the four event payloads live at [`ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/`](../../ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/):

| Topic | Message |
|---|---|
| `airdefenders/<site>/radar/detection/v1` | `airdefenders.v1.RadarDetection` |
| `airdefenders/<site>/missile/fired/v1` | `airdefenders.v1.MissileFired` |
| `airdefenders/<site>/missile/intercept/v1` | `airdefenders.v1.MissileIntercept` |
| `airdefenders/<site>/shahed/impact/v1` | `airdefenders.v1.ShahedImpact` |

A shared [`common.proto`](../../ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/common.proto) defines `Vec2` (used by every per-event message). All schemas use `proto3` syntax and `package airdefenders.v1;`. Field numbers are stable for v1 — never renumber; cut a `v2` topic key (`airdefenders/<site>/<event>/v2`) for any breaking change.

Build + upload + format-flip is wrapped by the final stage of [`ops/zenoh-trial-peers/bootstrap-airdefenders-portal.sh`](../../ops/zenoh-trial-peers/bootstrap-airdefenders-portal.sh): the script invokes `build-fds.sh` (protoc → `out/*.fds`), `upload-all.sh --set-format` (POST `.fds` + PATCH `topic.format` to `protobuf:<MsgName>`), and is idempotent on re-run (sha256-matching `.fds` bodies no-op; matching `topic.format` PATCH no-ops). `protoc` is operator-side, not bundled; the bootstrap stage SKIPs with a clear instruction when it's absent.

**Wire-format gap.** The publisher emits **JSON** on the wire (the envelope's `payload` map is JSON-encoded by `encoding/json` in [`ops/serve/airdefenders/zenoh.go`](../../ops/serve/airdefenders/zenoh.go)) even though the portal's `topic.format` reads `protobuf:airdefenders.v1.<MsgName>` after the format-flip. The portal's inspector "decode samples" path attempts to `dynamicpb`-decode the JSON bytes and fails per-sample; the schema viewer (`/schema.html`) and per-language autogen-code surfaces work because they walk the descriptor, not the wire. This is the same gap the synth-vendor schemas live with — see [CLAUDE.md](../../CLAUDE.md) under "Notional collapse" / "Synthetic vendor publishers". Flipping the wire format to genuine protobuf is a future task; the canonical payload list in §"Per-event payload shapes" above documents the JSON-on-wire shape.

### Feature flag

Zenoh publishing is opt-in via `ZENOH_PUBLISH=1` env var (default off). When off, the publisher goroutine is not started and the in-process EventBus has no Zenoh subscriber. With it on, the publisher requires `ZENOH_REST_ENDPOINT` (e.g. `http://airdefenders-zenoh-sidecar:8000` — the rest-pub-bridge sidecar, not the router directly) and `SITE_ID`. The `ZenohConfig` struct in [`ops/serve/airdefenders/zenoh.go`](../../ops/serve/airdefenders/zenoh.go) carries `RESTEndpoint` + `SiteID` + `Instance` — the cert-path fields from the prior CGo design (`CertPath` / `KeyPath` / `CAPath`) are gone with the REST pivot. nethost-hosted instance keeps `ZENOH_PUBLISH` unset and is unchanged.

## Failure modes

| Failure | Behaviour |
| --- | --- |
| WS client disconnects mid-tick | Tick goroutine ignores; the player is marked `disconnected=true` in the next tick's roster snapshot, `roster.update` is broadcast, reconnect slot held 30 s. The room continues if other players remain; pauses if this was the last player. |
| `input.fire` arrives with unknown `player_id` (i.e. server can't map the WS conn to a known player) | Drop silently. Caller should have completed `hello` handshake. Increment `airdefenders_mp_dropped_inputs_total{reason="no_player"}`. |
| `input.steer` for `interceptor_eid` not owned by sender | Drop silently. Increment `airdefenders_mp_dropped_inputs_total{reason="not_owner"}`. |
| `input.fire` from a player who owns no launcher (pre-spawn race / post-despawn) | Drop silently. Increment `airdefenders_mp_dropped_inputs_total{reason="not_owner"}`. The `launcher_idx` field is advisory on `input.fire` — server resolves the owned launcher itself — but if no launcher is owned, the verb is dropped. |
| `input.fire_mode` / `input.resupply` / `input.launcher_move` for a launcher not owned by sender (or out-of-bounds `launcher_idx`) | Drop silently. Increment `airdefenders_mp_dropped_inputs_total{reason="not_owner"}`. |
| `input.fire` above 30/sec/player | Drop the offending frame, emit one-shot `error{code:"rate_limited"}` to that client, do not close connection. |
| `input.launcher_move` above 30/sec/player | Drop silently (no error frame — keyboard-repeat saturation isn't actionable). Increment `airdefenders_mp_dropped_inputs_total{reason="rate_limited_launcher_move"}`. |
| `shouldCelebrate` DB read fails or times out (100 ms budget) | Log + fall through to GAME_OVER without CELEBRATION. The leaderboard auto-submit is unaffected (separate goroutine). |
| Player exceeds 4 conns/IP | New conn closed with 4002 before `hello`. |
| Zenoh router unreachable on `ZENOH_PUBLISH=1` startup | Log warning, mark subscriber `degraded=true`, retry connect every 30 s. Game loop unaffected. |
| Zenoh router goes away mid-game | Publisher drops frames, increments `airdefenders_mp_zenoh_publish_errors_total`, retries every 30 s. Game loop unaffected. |
| EventBus subscriber is slow (channel full) | Publish drops the event for that subscriber, increments `airdefenders_mp_eventbus_dropped_total{subscriber=...}`. Other subscribers and game loop unaffected. |
| Server shutdown (SIGTERM) | Close all WS with code 1001, give 5 s for graceful drain (existing `srv.Shutdown` path). Reconnect tokens lost (in-memory). |
| `state.tick` write times out to a client | Close that connection with 1011 (server error); reconnect-token recovery available. |
| Two players advance state simultaneously | First one to land in the tick wins; second is a no-op (server `spaceCooldown` re-set after first). |

## Metrics (additive to existing `/metrics`)

| Metric | Type | Meaning |
| --- | --- | --- |
| `airdefenders_mp_ws_connections` | gauge | Open WS connections |
| `airdefenders_mp_rooms` | gauge | Active rooms (1 in v1) |
| `airdefenders_mp_players` | gauge | Connected players |
| `airdefenders_mp_dropped_inputs_total` | counter (label: `reason`) | Inputs rejected |
| `airdefenders_mp_eventbus_dropped_total` | counter (label: `subscriber`) | EventBus drops to slow subscribers |
| `airdefenders_mp_zenoh_publish_errors_total` | counter | Zenoh publish failures |
| `airdefenders_mp_zenoh_publish_total` | counter (label: `event_type`) | Zenoh publishes attempted |
| `airdefenders_mp_tick_duration_seconds` | histogram | Server tick wall time |

## Auto-pilot personae

Client-side bots that play the game by reading the same `state.tick` snapshots a human sees and sending the same input verbs. The server cannot distinguish a bot from a human — there is **no** server-side bot code, no new verb, no protocol change. A bot is purely a parameter set over one heuristic loop in `mp.html`:

> each decision: pick the most threatening live shahed (the one closest to the ground), slide the launcher under its predicted intercept-X, and fire when lined up and off cooldown.

Every persona is that same loop with different knobs (`PERSONAS` in `mp.html`): `mode` (preferred `fire_mode` to hold), `fireMs` (min ms between fires), `aimTol` (px alignment gate before firing), `aimNoise` (px jitter), `moveSpeed` (fraction of the ±6 px/tick step), `reactionMs` (decision cadence), `churn` (probability of picking a random target instead of the most urgent), `vis` (ignore radar-invisible shaheds), `resupplyAt`, `steer` (manually steer the in-flight interceptor), plus a few persona-specific flags (`modeFlip`, `napProb`/`napMs`, `roam`, `useCamel`, `holdCenter`).

| Persona | Plays like | Defining knobs |
| --- | --- | --- |
| **Trigger** | trigger-happy | autopilot mode, fires every ~110 ms, loose aim, ammo hog — drives `missile.fired` volume |
| **Deadeye** | sharpshooter / perfectionist | manual mode, tight aim gate (13 px), patient (~620 ms), manually steers, high hit ratio |
| **Gremlin** | chaotic | heavy aim jitter (130 px), 50% target churn, randomly cycles `fire_mode` |
| **Dozer** | sleepy | slow decisions (430 ms), slow movement, periodic "naps" (idles for ~2.6 s) |
| **Sloppy** | messy | autopilot, aim noise (75 px), over-fires, frequent target switching |
| **Squint** | only hits what it can see | ignores shaheds with `visible <= 0.1`, manual + steers, engages on-screen threats only |
| **Nomad** | roamer | sweeps the launcher across the whole field (sine), sprays autopilot missiles — drives `input.launcher_move` |
| **Camel** | themed support | holds mid-field, conservative fire, leans on `input.camel_release` + bunker runs |

The eight collectively exercise every input verb (fire, launcher_move, steer, fire_mode, resupply, state_advance, bunker_run, camel_release), so a full fleet produces a rich, continuous flow across all four Zenoh event types — useful for watching the fabric over `goat sub --tui airdefenders/**`, panoscope, stethoscope, and flashlight. Step-by-step operator runbook for the full demo (spawn the fleet + watch all four surfaces at once, with the verified commands/URLs): [`docs/operations/airdefenders-zenoh-demo.md`](../operations/airdefenders-zenoh-demo.md).

**Enable.** `?bot=<persona>` on the `/mp` URL (e.g. `/mp?bot=deadeye`) auto-plays; `?auto=1` alone defaults to Trigger. Or `BOT.enable('nomad')` from devtools. A bot auto-advances through TITLE → EMPLACEMENT → WAVE_END (needs zero clicks), asserts its persona name into the roster, and sets `document.title = "AD bot · <Persona>"` so windows are identifiable. The bot tick is hooked into the existing `render()` rAF loop and respects the same client-side send cadences (fire/move ≤ 30 Hz, steer ≤ 10 Hz).

**Spawn a full fleet.** [`ops/serve/airdefenders/spawn-bots.sh`](../../ops/serve/airdefenders/spawn-bots.sh) opens 8 Chrome windows in a grid, one persona each, every window on a throwaway `--user-data-dir` profile so it is a fully independent player (separate WebSocket + sessionStorage) that never touches your real Chrome profile. Eight bots fill the room (`maxPlayers = 8`); watch the renders + the fabric externally. Usage:

```
ops/serve/airdefenders/spawn-bots.sh                  # 8 bots vs localhost:8095
ops/serve/airdefenders/spawn-bots.sh https://airdefenders.<site>.<zone>
AD_BOTS="trigger deadeye nomad" ops/serve/airdefenders/spawn-bots.sh   # subset
ops/serve/airdefenders/spawn-bots.sh --kill           # close windows + remove temp profiles
```

Because all eight connect from one laptop IP, the per-IP cap matters — the default `defaultMaxConnsPerIP = 8` already admits a full fleet; set `AD_MAX_CONNS_PER_IP=9` on the server if you also want to join as a human.

## One-page demo (`/demo`)

`GET /demo` ([`ops/serve/airdefenders/demo.html`](../../ops/serve/airdefenders/demo.html), served by `handleDemoPage`) is the no-script, any-OS way to run the full fleet: it renders the eight personae as same-origin `<iframe src="/mp?bot=<persona>">` cells in a grid, so the whole co-op room (`maxPlayers = 8`) fills from one browser tab. Its top bar shows the `goat sub 'airdefenders/<site>/**' --follow` watch command (with a copy button) and deep-links to flashlight / panoscope / stethoscope focused on airdefenders; `SITE_ID` is substituted server-side and the surface hostnames are derived client-side from this page's own host (the sibling-surface label swap), so it's correct on any goatnet with zero per-site config.

**sessionStorage namespacing.** `sessionStorage` is shared across all same-origin frames in one tab (keyed by the top-level browsing context), so the per-session keys `mp_reconnect_token` / `mp_name` would collide — every bot frame would read and stomp the same reconnect token and fight over one player slot. When a bot persona is set (`?bot=…`), `mp.html` suffixes those keys per-persona (`mp_reconnect_token::trigger`, …) so each frame keeps its own. Single-window play (no `?bot=`) keeps the bare keys, so a human's reconnect token survives a reload exactly as before.

**Per-IP cap.** The eight frames use exactly the eight per-IP WebSocket slots (`defaultMaxConnsPerIP = 8`); the `/demo` wrapper itself opens no WebSocket. Don't also open a standalone game tab from the same machine while the demo runs, or the 9th connection is refused.

**Who may open it (2026-09-11).** `/demo` fills the whole room, so it is not a game mode: [`demo_auth.go`](../../ops/serve/airdefenders/demo_auth.go) gates it to platform admins and draws a **▶ RUN FABRIC DEMO (admin)** button on both title screens (`/` and `/mp`) for them and nobody else. The identity comes from the authenticated door ([ADR 1073](../adr/1073-rbac-adherence-principle.md)): oauth2-proxy sets `X-Forwarded-User` / `-Email` / `-Groups` from the validated session and strips that family from client input, so the game reads it only when the compose sets `TRUST_FORWARDED_IDENTITY=1` — which it does exactly when `airdefenders_oauth2_proxy_enabled` is true. Any group in `DEMO_ADMIN_GROUPS` (group_var `airdefenders_demo_admin_groups`, default `PLATFORM-ADMIN,ADMINS`) counts. With no door the headers are untrusted, nobody is an admin, the button never renders and `/demo` answers 403 — closed rather than open; `AD_DEMO_UNGATED=1` is the laptop-only escape hatch and Ansible never sets it. `GET /api/me` (`{authenticated, demo_admin, user}`) is how the page learns which case it is in; it lives under `/api/` so the door treats it as an XHR (401, no login redirect, no CSRF-cookie churn — F-342). The matrix row (`ops/permissions/group-permissions.yml`) records this as the one `admin` grant on an otherwise any-login door. One click on the button opens `/demo` in a named tab (a second click focuses it instead of starting a second fleet); closing the tab drops the bots.

`goat demo airdefenders` ([ADR 1019](../adr/1019-goat-cli-unified-operator-binary.md) CLI) is the one-command convenience over this page: it derives the demo URL + site from the active profile, opens `/demo` in the browser, and runs the `goat sub` watch in the terminal — both halves of the demo at once, cross-platform (it opens a URL, not eight positioned windows). See [`docs/operations/airdefenders-zenoh-demo.md`](../operations/airdefenders-zenoh-demo.md).

## Out of scope (v1)

- **Anti-cheat signing.** Mesh ACL is the auth surface, same posture as `/api/scores`. Server-authoritative state already blocks edit-JS cheats; spammed valid `input.fire` is rate-limited.
- **Delta snapshots.** Not built. Measured on zagros-dev 2026-09-11 (F-521): full `state.tick` snapshots at 30 Hz cost 129 kbit/s per socket in the lobby and ~1 Mbit/s per socket in combat (p95 frame 10 KB). Eight sockets with one player firing cost 7.8 Mbit/s, so an open `/demo` tab costs at least that. Delta encoding is one of two levers if that cost has to fall. The follow-on push to 16 KiB read-limit was the breathing room for the new `path[]` / camel / bunker fields; delta encoding is the next lever if measured load demands it.
- **Per-player named identities beyond display name.** No Kratos integration; names are unverified strings.
- **Cross-site federation.** Single `SITE_ID` per server. Multi-site joint games would need site-aware room IDs.
- **Mobile input parity for steering.** Existing touch model assumes one active interceptor; per-player active-interceptor HUD is Agent 2's call (implement or defer).
- **Private rooms / matchmaking.** One global room.
- **Spectator mode.** Every `hello` becomes a player.
- **State persistence across server restarts.** Restart resets to `TITLE`; reconnect tokens are in-memory.
- **WebSocket compression.** Disabled (`ws.go:95`). It is the other lever for the per-socket cost in F-521, and its saving is unmeasured.
- **NTP attestation in envelope.** Deliberately deferred to Block 11 Step 9; that block introduces a reusable `telemetry-client` Go module every fabric publisher will import, so wiring `chronyc` into airdefenders here would build the exact "airdefenders-only code" Step 9 was scoped to prevent. The envelope's comment line spells this out at the source.
- **Confetti physics on the server.** CELEBRATION transmits only the phase flag + paused state; the confetti particle system lives entirely in the renderer (Agent 2). This keeps the wire format size unchanged during CELEBRATION (`shaheds`/`interceptors`/everything-else are frozen at the final COMBAT frame).

The follow-on (2026-05-19) landed every "MAY DEFER" item from the original Phase 2 doc — CELEBRATION phase, camel mode, bunker run, per-entity `path[]`, kill snapshots via the new `wave.summary` frame, `input.launcher_move`, full 7-pattern shahed dispatcher, cruise straight-line kinematics. The "deferred mechanics" subsection that previously listed these is gone.

## Post-MVP changes (2026-05-22)

A round of live-test feedback drove a tightly-scoped follow-on that touches the UX surface but leaves the WS protocol stable. Five changes in this slice:

1. **SP/MP leaderboards split.** A new `mode` column on the `scores` table separates single-player and multiplayer submissions; both clients pass `?mode=sp` / `?mode=mp` on the existing `GET /api/scores` and the SP `POST` body carries the same field. The schema migration is additive (`PRAGMA table_info`-probe + `ALTER TABLE … ADD COLUMN`); pre-migration rows default to `mode='sp'`. The CELEBRATION-gate `queryTop` on the room tick filters `mode='mp'` so an SP top-5 doesn't trigger MP celebration. Per-mode design: [`airdefenders-leaderboard.md`](airdefenders-leaderboard.md) §"Data model" + §"Period filtering".
2. **Team name auto-derived (no user prompt).** An earlier slice prototyped a typed team-name workflow with a new `input.submit_team` WS verb; that path was reverted entirely. The server now writes `team` at insert time as `<topName>+<connected-1>` (or just `<topName>` solo). `members` carries the comma-joined contributor names in rank order. The high-score panel renders both. No user input, no `team.submitted` broadcast, no race window between submitters.
3. **GAME_OVER → TITLE on advance (not auto-restart).** `gameState.advanceState`'s `case phaseGameOver` was `reset() → startEmplacement()`; it now does `reset() → phase = phaseTitle`. The next SPACE press on TITLE starts a fresh round via the existing TITLE-state branch. Player sees the home screen + refreshed leaderboard + name-edit affordance between games rather than being thrown straight into emplacement. Regression test: `TestAdvanceFromGameOverLandsOnTitle`.
4. **Per-player name editing on TITLE.** `mp.html` (and `index.html`) now render a small canvas `EDIT NAME (<current>)` button on TITLE that pops the existing `nameInput` overlay; commit on Enter / blur persists to `localStorage.airdefenders_name` and (MP) `sessionStorage.mp_name`, then fires `MP.send('input.set_name', { name })` so the server-side roster picks up the rename without a reconnect (the verb was already supported via `decodeInput` + `applyInput`'s `input.set_name` case). MP TITLE high-score panel refreshes after `game.over` so a player's just-submitted run shows up immediately.
5. **HTML touch-controls strip below the canvas.** Both pages wrap the canvas in `<div id="frame">` and add a sibling `<div id="touchControls">` (CSS `display: none` by default; `.touch` class added at boot when `isTouchDevice`). The strip carries hold-to-move arrow buttons (◀ MOVE / MOVE ▶), MODE / RESUPPLY / FIRE / ADVANCE (context-sensitive label: START / READY / NEXT WAVE / HOME / COMBAT-disabled), and a SWITCH button on SP that surfaces only when `launchers.length >= 2`. The arrow buttons proxy to the same key state the keyboard uses — SP writes `keys['KeyA']` / `keys['KeyD']`, MP adds `'KeyA'` / `'KeyD'` to the existing `heldKeys` Set — so the existing per-frame movement code (SP) / `pollHeldKeys` 30 Hz emit (MP) runs unchanged. No throttling, no slider feedback loop, exact same speed and feel as keyboard A/D. `pointerdown` + `setPointerCapture` + `pointerup` / `pointercancel` / `pointerleave` guarantee a finger that slides off the button still ends the held state. `resizeCanvas` accounts for the strip's height when computing scale, so removing the on-canvas mobile-only button regions (`BTN_FIRE_MODE` / `BTN_RESUPPLY`) from the canvas surface lets the canvas letterbox larger on mobile.

The WS schema is unchanged from the post-2026-05-19 baseline — no new verbs, no removed verbs (the `input.submit_team` verb from the abandoned team-name slice is gone). Client-side state in `mp.html`'s `MP` object loses the `teamSubmitState` / `teamSubmitted` / `teamMembers` fields and gains `highScores` / `highScoresFetchedAt`. The `nameInput` DOM element is reused for both SP-on-GAME_OVER (existing flow, unchanged) and SP+MP-on-TITLE (new flow).

## References

- [ops/serve/airdefenders/main.go](../../ops/serve/airdefenders/main.go) — existing server, untouched endpoints
- [ops/serve/airdefenders/index.html](../../ops/serve/airdefenders/index.html) — world constants L29-30; input verbs L765-803; entity factories L880-924; game loop L1872; fire/resupply L1844-1862
- [ops/serve/airdefenders/mp.html](../../ops/serve/airdefenders/mp.html) — multiplayer client; `PERSONAS` + `BOT` auto-pilot module near the boot block (§Auto-pilot personae)
- [ops/serve/airdefenders/ws.go](../../ops/serve/airdefenders/ws.go) — `maxConnsPerIP()` resolves `AD_MAX_CONNS_PER_IP` (default 8)
- [ops/serve/airdefenders/spawn-bots.sh](../../ops/serve/airdefenders/spawn-bots.sh) — open one Chrome window per persona in a grid
- [ops/serve/airdefenders/demo.html](../../ops/serve/airdefenders/demo.html) — the `/demo` one-page fabric demo (8 persona iframes; §One-page demo)
- [docs/design/airdefenders-leaderboard.md](airdefenders-leaderboard.md) — leaderboard + Block 11 Step 9 plan
- [docs/adr/0844-zenoh-deployment-topology-and-acl-v1.md](../adr/0844-zenoh-deployment-topology-and-acl-v1.md) — reserved prefixes; vendor namespace is outside reserved
- [docs/design/nova-x509-identity.md](nova-x509-identity.md) — per-vendor cert minting
- [docs/design/zenoh-trial-fabric-synth-vendors.md](zenoh-trial-fabric-synth-vendors.md) — vendor-naming convention
