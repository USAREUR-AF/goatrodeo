// Game — server-authoritative physics, ported from index.html.
//
// All per-frame timers in index.html assume 60 Hz; this server runs 30 Hz,
// so timer constants are halved. The schema doc §"Tick model" documents
// the mapping. Whether the snapshot encodes "frames" or "ticks" externally
// doesn't matter as long as the wall-clock interpretation matches: e.g.
// ARM_TIME at 30 Hz = 30 ticks = 1 second, same wall-clock as index.html.
//
// What we port from index.html (line citations to that file):
//   - Shahed spawn, all 7 flight patterns + cruise straight-line (L961-1006, L884-908)
//   - Interceptor spawn + flight + drag + speed clamp + proximity-fuse blast kill
//   - Launchers: fire, arm timer, resupply timer, fire_mode 0/1/2, launcher_move
//   - Radars: in-circle visibility tracking, 0→positive radar.detection event
//   - Lives, score, wave progression, intercept counter, life-up threshold
//   - Phase state machine: TITLE → EMPLACEMENT → COMBAT → WAVE_END →
//     (EMPLACEMENT | CELEBRATION → GAME_OVER) (L437-442, L2579-2580)
//   - Camel mode at wave 6 + camel spit + retreating shahed (L559-665, L1805-1809)
//   - Bunker run + warning + post-phase outcomes (L540-735, L1819)
//   - Per-entity path[] for trajectory review (L1006 — recorded every 3 ticks)
//   - Kill snapshots (per-kill) emitted in a wave.summary frame at WAVE_END / GAME_OVER
//   - Snapshot marshalling matching schema doc §state.tick
//   - Event emission at the 4 documented points (radar.detection,
//     missile.fired, missile.intercept, shahed.impact)
package main

import (
	"math"
	"math/rand"
	"time"
)

// ─── Phase constants ───────────────────────────────────────
type phase string

const (
	phaseTitle        phase = "TITLE"
	phaseEmplacement  phase = "EMPLACEMENT"
	phaseCombat       phase = "COMBAT"
	phaseWaveEnd      phase = "WAVE_END"
	phaseGameOver     phase = "GAME_OVER"
	phaseCelebration  phase = "CELEBRATION"
)

// ─── Physics constants ─────────────────────────────────────
// All "frame" constants from index.html halved for 30 Hz.
const (
	armTime          = 30  // 60 / 2 — 1 second
	autopilotArmTime = 60  // 120 / 2 — 2 seconds
	resupplyTime     = 60  // 120 / 2 — 2 seconds
	proxRadius       = 147 // unchanged — spatial
	proxTrigger      = 36  // ~ proxRadius / 4
	autopilotDetect  = 294 // proxRadius * 2
	groundY          = worldH - 50

	// dualLauncherWave is retained as a no-op compile-time anchor in case
	// downstream consumers grep for it; it has no runtime effect now that
	// launchers are per-player. Pre-rewrite the game spawned a second
	// launcher at wave 4 (index.html L1784) — that mechanic is replaced by
	// the per-player ownership model where every connected player owns one
	// launcher and the count tracks the connected-player count.
	dualLauncherWave = 4
	maxLives         = 5
	maxRadars        = 6
	minRadars        = 3

	// maxLauncherSlots — 8 reserved X positions, one per possible player
	// (matches maxPlayers in multiplayer.go). Slot indexes are stable for
	// the lifetime of the room — a player who reconnects within the 30 s
	// window lands in the same slot, so their launcher does not visually
	// migrate. The schema-doc §"Shared state model" + §"Rooms and session
	// lifecycle" call out this slot-reuse contract.
	maxLauncherSlots = 8

	// CELEBRATION — index.html L389-442. 120 frames at 60 Hz = 60 ticks at 30 Hz.
	celebrationTicks = 60

	// Bunker — index.html L545-550. Halved for 30 Hz.
	// bunkerCharStartX + bunkerX are renderer-side concerns; the server only
	// emits run_timer/timer and the renderer interpolates the character along
	// the line between those world-coords.
	bunkerRunWave       = 5
	bunkerWarningTicks  = 300 // 600 / 2 — 10 s
	bunkerRunTicks      = 150 // 300 / 2 — 5 s
	bunkerPostTicks     = 45  // 90 / 2 — 1.5 s

	// Camel — index.html L562-571. Spatial constants unchanged; time halved.
	camelWave          = 6
	camelPoolStart     = 12
	camelSpitRangeX    = 90.0
	camelSpitRangeY    = 320.0
	camelSpitCooldown  = 38 // 75 / 2

	// Path[] — index.html L1006. We record one sample per tick during COMBAT;
	// 30 ticks = 1 second of trajectory. Per-entity ring buffer.
	pathRingLen = 30

	// launcher_move clamp range — sized so one tick worth of input reproduces
	// single-player's 60 fps × 3 px/frame = 180 px/s. At 30 Hz that's 6 px/tick,
	// so clamp at ±6 and a client sending one dx=6 per tick matches the
	// single-player keyboard feel exactly. Lower values feel sluggish.
	launcherMoveMaxDX = 6.0
	launcherMoveRateLimit = 60 // per second per player; tolerates 30 Hz send + headroom
)

// ─── Entities ──────────────────────────────────────────────

type shahed struct {
	EID       uint32
	X, Y      float64
	BaseX     float64
	PrevX     float64 // for angle / vx; matches index.html s.prevX
	VX, VY    float64
	Speed     float64
	Amplitude float64
	Frequency float64
	PhaseOff  float64
	Wobble    float64
	Cruise    bool
	Pattern   int
	T         int
	Alive     bool
	Visible   float64
	// Pattern-5 drift (set lazily on first updateShahed call). Matches index.html
	// s.drift; one direction × magnitude, fixed for the shahed's lifetime.
	Drift float64
	// Retreating — set true when a camel spit hits; shahed flies up and off.
	Retreating  bool
	RetreatVX   float64
	RetreatAccel float64
	// Path — ring buffer of the last pathRingLen positions; populated only
	// during COMBAT. See schema doc §"per-entity path".
	Path *pathRing
	// FullPath — unbounded trail across the entity's lifetime, populated at
	// the same cadence as Path but never capped. NOT sent in state.tick
	// (would balloon bandwidth); used at the death site to snapshot the full
	// trajectory into wave.summary.shaheds[].path so the WAVE_END review
	// shows the entire flight, not just the last 2 seconds of ring history.
	FullPath []pathPoint
}

type interceptor struct {
	EID           uint32
	X, Y          float64
	VX, VY        float64
	Alive         bool
	Armed         bool
	AutoPilot     bool
	Age           int
	OwnerPlayerID uint32
	// AimX/AimY — if non-zero, manual steering target from input.steer.
	AimX, AimY float64
	HasAim     bool
	// LauncherIdx — for missile.fired event payload only.
	LauncherIdx int
	// Path — see shahed.Path.
	Path *pathRing
	// FullPath — see shahed.FullPath. Unbounded; emitted in wave.summary at death.
	FullPath []pathPoint
}

// camel — ground unit released in wave 6. Walks horizontally, spits at the
// nearest threatening shahed. Ported from index.html L601-660.
type camel struct {
	EID          uint32
	X            float64
	VX           float64
	Dir          int // 1 = walking right, -1 = walking left; from sign(VX)
	Bob          float64
	SpitCooldown int
	Age          int
	Alive        bool
}

// camelSpit — projectile fired by a camel. Hits flip a shahed into retreat.
type camelSpit struct {
	EID   uint32
	X, Y  float64
	VX, VY float64
	Age   int
	Alive bool
}

// pathRecord — one entity's full trajectory + the outcome it ended on.
// Accumulated during COMBAT per shahed/interceptor death and emitted in the
// wave.summary frame at WAVE_END / GAME_OVER. Mirrors single-player's
// trajectories.shaheds[] / trajectories.interceptors[] (index.html
// L720-L725, L2080-L2092). Outcome strings match solo verbatim:
//
//   shahed:       "intercepted" | "impact" | "retreated"
//   interceptor:  "hit"         | "miss"
type pathRecord struct {
	Path    []pathPoint
	Outcome string
}

// killSnapshot — recorded when an interceptor kills a shahed. Emitted in the
// wave.summary frame at WAVE_END / GAME_OVER. Mirrors index.html's
// recordKillSnapshot (L867-878) but flat shape for the wire.
type killSnapshot struct {
	ShahedEID uint32
	KillX     float64
	KillY     float64
	PlayerID  uint32
	Tick      uint32
	// Path — copy of the killed shahed's path[] at the moment of kill.
	// We commit to "final pathRingLen positions" per design doc §wave.summary.
	Path []pathPoint
	// WorldShaheds — snapshot of all OTHER alive shaheds at the kill instant.
	// The renderer uses this to recreate the moment-of-kill freeze frame —
	// solo (index.html L1648-L1700) iterates `snap.shaheds` for the same reason.
	// We exclude the killed shahed itself (its position is KillX/KillY and a
	// dedicated marker renders there). Empty when no other shaheds were alive.
	WorldShaheds []shahedSnap
}

// shahedSnap — per-shahed state captured at a kill instant. Enough fields to
// drive drawShahedShape / drawCruiseShape on the client at the correct
// position + orientation.
type shahedSnap struct {
	EID    uint32
	X, Y   float64
	VX, VY float64
	Cruise bool
}

// pathPoint — one (x,y) sample in a per-entity path[] history.
type pathPoint struct {
	X, Y float64
}

// pathRing — fixed-capacity ring buffer of pathPoint, capacity pathRingLen.
// We allocate one per entity. Single-goroutine access (tick loop), no lock.
type pathRing struct {
	buf  [pathRingLen]pathPoint
	head int // next write index
	n    int // count of valid entries (≤ pathRingLen)
}

func (p *pathRing) push(x, y float64) {
	p.buf[p.head] = pathPoint{X: x, Y: y}
	p.head = (p.head + 1) % pathRingLen
	if p.n < pathRingLen {
		p.n++
	}
}

// snapshot — returns the ring's contents in chronological order (oldest first).
// Allocates a fresh slice each call; callers that need to retain a copy own it.
func (p *pathRing) snapshot() []pathPoint {
	if p == nil || p.n == 0 {
		return nil
	}
	out := make([]pathPoint, p.n)
	start := p.head - p.n
	if start < 0 {
		start += pathRingLen
	}
	for i := 0; i < p.n; i++ {
		out[i] = p.buf[(start+i)%pathRingLen]
	}
	return out
}

// reset — empty the buffer; called on phase transitions out of COMBAT so
// EMPLACEMENT-time radar drag doesn't pollute trajectories.
func (p *pathRing) reset() {
	p.head = 0
	p.n = 0
}

// launcher — per-player ownership v1. Each launcher belongs to exactly one
// player; a player owns at most one launcher. Ownership is conferred at join,
// freed at disconnect-timeout, preserved across reconnects. See schema doc
// §"Shared state model".
type launcher struct {
	X                float64
	InterceptorsLeft int
	FireMode         int
	Arming           bool
	ArmTimer         int
	Resupplying      bool
	ResupplyTimer    int

	// OwnerPlayerID — set on spawn. 0 means "unowned" (only possible during
	// the pre-multiplayer TITLE state; once any player joins, every launcher
	// in the snapshot has a non-zero owner).
	OwnerPlayerID uint32

	// Slot — index into the maxLauncherSlots reserved-X-position table.
	// Stable across reconnects so a returning player's launcher sits where
	// it sat before the disconnect. Range [0, maxLauncherSlots).
	Slot int

	// ArmingByPlayerID — set when fireFromLauncher kicks off an arming
	// sequence; consumed by launchInterceptor on arm-timer expiry so the
	// missile.fired event + interceptor's OwnerPlayerID carry the arming
	// player. Per-player ownership ensures this equals OwnerPlayerID, but
	// keeping a dedicated field decouples the two if the contract ever
	// changes. Cleared after the spawn fires.
	ArmingByPlayerID uint32
}

type radar struct {
	X, Y, Radius float64
}

type explosion struct {
	X, Y  float64
	Color string
	Age   int
}

type blastWave struct {
	X, Y      float64
	Radius    float64
	MaxRadius float64
}

// ─── gameState ─────────────────────────────────────────────

// gameState — owned by the Room's tick goroutine; never share-mutated.
type gameState struct {
	bus *eventBus

	phase phase
	wave int

	score      int64
	lives      int
	intercepts int

	waveTimer    int
	waveEndTimer int

	spawnTimer     int
	shahedsToSpawn int

	launchers    []*launcher
	radars       []*radar
	shaheds      []*shahed
	interceptors []*interceptor

	// ephemerals — included in snapshot then aged out
	explosions []*explosion
	blastWaves []*blastWave

	// flash counters (ticks remaining; client renders directly)
	killFlash   int
	lifeUpFlash int

	// monotonic EID minter
	nextEID uint32

	// has the game.over frame been emitted yet for this game?
	gameOverFired bool

	// ─── Bunker (Block ported from index.html L545-735) ───
	// One bunker event lifecycle: idle → active → post → idle. Per-wave schedule
	// from scheduleBunkerEvents(); skipped on camel waves and pre wave-5 waves.
	bunkerPhase     string // "idle" | "active" | "post"
	bunkerTimer     int    // active: ticks remaining of warning; post: ticks remaining of post-phase
	bunkerRunTimer  int    // 0 until input.bunker_run; then ticks since
	bunkerOutcome   string // "pending" | "safe" | "hit"
	bunkerPostTimer int
	bunkerSchedule  []int // waveTimer values to trigger events
	bunkerDone      int

	// ─── Camel mode (Block ported from index.html L559-665) ───
	camelMode         bool
	camels            []*camel
	camelSpits        []*camelSpit
	camelPool         int
	camelReleaseTimer int

	// ─── CELEBRATION (Block ported from index.html L437-442, L2579-2580) ───
	celebrationTimer int

	// ─── Kill snapshots (collected during COMBAT for wave.summary) ───
	killsThisWave []killSnapshot
	leakedShaheds int // shahed.impact count for current wave

	// ─── Per-wave path log — flight trajectories + outcomes for the WAVE_END /
	// GAME_OVER review screen. Solo accumulates these in a `trajectories` object
	// (index.html L720, L2080-L2092); we accumulate on the server so the client
	// renders the same green/red/blue/gray color-coded paths at wave end.
	wavePaths struct {
		Shaheds      []pathRecord
		Interceptors []pathRecord
	}

	// attributeFn — optional callback set by the owning Room. Invoked
	// when a kill is attributed to a player, so the Room can bump the
	// per-player Intercepts counter on its Player struct without needing
	// a back-pointer from gameState into the player map.
	attributeFn func(playerID uint32, n int)

	// onWaveSummary — optional callback emitted on phaseCombat → phaseWaveEnd
	// and again on phaseWaveEnd → phaseGameOver, carrying killsThisWave + the
	// leak counter + every shahed/interceptor's path so the Room can
	// broadcast a wave.summary frame. killsThisWave + leakedShaheds reset
	// after each emission; wavePaths persists until next startCombat so the
	// WAVE_END→GAME_OVER frame still carries the paths.
	onWaveSummary func(wave int, kills []killSnapshot, leaked int, shahedPaths, interceptorPaths []pathRecord)

	// time-since-last-state-change advance cooldown lives on the player.

	rng *rand.Rand
}

func newGameState(bus *eventBus) *gameState {
	g := &gameState{
		bus: bus,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	g.reset()
	return g
}

// reset — back to TITLE with default lives + score.
//
// Per-player launchers PERSIST across reset (GAME_OVER → TITLE) as long as the
// owner stays connected. Launchers are owned by Players, not by the current
// game — Agent 1's per-player ownership refactor moved their lifecycle to
// {spawn-on-join, despawn-on-disconnect-timeout}, so a per-game reset must
// NOT touch the slice or the launcher's identity fields (OwnerPlayerID, Slot,
// X). What we DO clear here is the per-game ephemeral state — in-flight arm /
// resupply timers — and we refill ammo to a generous default so the post-
// reset UI shows full launchers (startCombat re-allocates per-wave anyway).
// FireMode is the player's deliberate choice and persists across resets.
//
// Schema doc §"Rooms and session lifecycle" documents the persistence
// contract; see also docs/design/airdefenders-multiplayer.md.
func (g *gameState) reset() {
	g.phase = phaseTitle
	g.wave = 0
	g.score = 0
	g.lives = maxLives
	g.intercepts = 0
	g.waveTimer = 0
	g.waveEndTimer = 0
	g.spawnTimer = 0
	g.shahedsToSpawn = 0
	// Launchers persist across reset (see docstring). Clear per-game
	// ephemerals (arming + resupply in-flight) and refill ammo. Identity
	// fields (OwnerPlayerID, Slot, X) and the player's FireMode choice
	// survive untouched.
	const postResetAmmo = 8 // matches spawnLauncherForOwner's initial budget
	for _, L := range g.launchers {
		L.Arming = false
		L.ArmTimer = 0
		L.ArmingByPlayerID = 0
		L.Resupplying = false
		L.ResupplyTimer = 0
		L.InterceptorsLeft = postResetAmmo
	}
	g.radars = nil
	g.shaheds = nil
	g.interceptors = nil
	g.explosions = nil
	g.blastWaves = nil
	g.killFlash = 0
	g.lifeUpFlash = 0
	g.gameOverFired = false
	g.bunkerPhase = "idle"
	g.bunkerTimer = 0
	g.bunkerRunTimer = 0
	g.bunkerOutcome = "pending"
	g.bunkerPostTimer = 0
	g.bunkerSchedule = nil
	g.bunkerDone = 0
	g.camelMode = false
	g.camels = nil
	g.camelSpits = nil
	g.camelPool = 0
	g.camelReleaseTimer = 0
	g.celebrationTimer = 0
	g.killsThisWave = nil
	g.leakedShaheds = 0
}

func (g *gameState) nextID() uint32 {
	g.nextEID++
	return g.nextEID
}

// ─── State machine ─────────────────────────────────────────

// advanceState — implements the space-key state transitions.
// Returns true if a transition occurred (caller resets per-player cooldown).
func (g *gameState) advanceState() bool {
	switch g.phase {
	case phaseTitle:
		g.startEmplacement()
		return true
	case phaseEmplacement:
		g.startCombat()
		return true
	case phaseWaveEnd:
		if g.waveEndTimer > 60 {
			g.startEmplacement()
			return true
		}
	case phaseGameOver:
		// Return to TITLE rather than auto-starting the next round so the
		// player sees the home screen + leaderboard + name-edit affordance
		// before committing to another game. The next SPACE press on TITLE
		// goes through the phaseTitle branch above which calls
		// startEmplacement(). Reset wipes per-game state + refills ammo;
		// launchers (owned by players) persist per
		// TestLaunchersPersistAcrossGameOverReset.
		g.reset()
		g.phase = phaseTitle
		return true
	}
	return false
}

func (g *gameState) startEmplacement() {
	g.wave++
	g.phase = phaseEmplacement
	g.shaheds = nil
	g.interceptors = nil
	// Launcher count is now driven by connected-player count, not wave.
	// (Pre-rewrite a second launcher was added at wave 4 per index.html
	// L1784; the per-player ownership model replaces that.)
	// Ensure enough radars.
	want := minRadars + (g.wave-1)/2
	if want > maxRadars {
		want = maxRadars
	}
	for len(g.radars) < want {
		g.radars = append(g.radars, &radar{
			X:      100 + g.rng.Float64()*(worldW-200),
			Y:      150 + g.rng.Float64()*(worldH-350),
			Radius: 90 + math.Min(float64(g.wave*3), 30),
		})
	}
	// Grow existing radar range slightly each wave (clamp 140).
	maxR := math.Min(float64(90+g.wave*4), 140)
	for _, r := range g.radars {
		r.Radius = maxR
	}
}

func (g *gameState) startCombat() {
	g.phase = phaseCombat
	// Camel mode at wave 6 — index.html L1805-1810.
	g.camelMode = g.wave == camelWave
	g.camels = nil
	g.camelSpits = nil
	if g.camelMode {
		g.camelPool = camelPoolStart
		g.camelReleaseTimer = 22 // 45 / 2; first auto-release ~0.75s into combat
		g.shahedsToSpawn = 20    // matches index.html L1810 camel-wave count
	} else {
		g.camelPool = 0
		g.camelReleaseTimer = 0
		g.shahedsToSpawn = 2 + g.wave*2
	}
	g.spawnTimer = 0
	g.shaheds = nil
	g.interceptors = nil
	g.waveTimer = 0
	g.killsThisWave = g.killsThisWave[:0]
	g.leakedShaheds = 0
	g.wavePaths.Shaheds = g.wavePaths.Shaheds[:0]
	g.wavePaths.Interceptors = g.wavePaths.Interceptors[:0]
	g.scheduleBunkerEvents()
	// Per-player ownership: each launcher gets the full per-launcher budget,
	// so total interceptors on the field scale linearly with connected
	// players. (Pre-rewrite the budget was a fixed pool split across the
	// 1-2 shared launchers — divide-by-zero on zero launchers; new shape
	// is allocation-per-launcher so a solo player gets the full pool and an
	// N-player room gets N pools.)
	perLauncher := int(math.Ceil(float64(g.shahedsToSpawn) * 1.5))
	if len(g.launchers) > 1 {
		// In a multi-player room each player has their own share; tighten
		// the per-launcher budget so the total stays roughly proportional
		// to wave difficulty rather than ballooning with connected count.
		// Solo (1 launcher) keeps the full per-launcher count = old shape.
		perLauncher = int(math.Ceil(float64(g.shahedsToSpawn) * 1.5 / float64(len(g.launchers))))
		if perLauncher < 2 {
			perLauncher = 2
		}
	}
	// Per-launcher per-wave state: refill ammo, clear any in-flight
	// arming/resupply (those timers were tied to the wave that just
	// ended). FireMode is the player's deliberate selection and persists
	// across waves + across GAME_OVER resets — schema doc §"Shared state
	// model".
	for _, L := range g.launchers {
		L.Arming = false
		L.ArmTimer = 0
		L.ArmingByPlayerID = 0
		L.Resupplying = false
		L.ResupplyTimer = 0
		L.InterceptorsLeft = perLauncher
	}
}

// scheduleBunkerEvents — index.html L668-683. Per-wave bunker-event schedule;
// idle on pre-wave-5 waves and on the camel wave.
func (g *gameState) scheduleBunkerEvents() {
	g.bunkerSchedule = nil
	g.bunkerDone = 0
	g.bunkerPhase = "idle"
	g.bunkerOutcome = "pending"
	g.bunkerRunTimer = 0
	if g.wave < bunkerRunWave {
		return
	}
	if g.wave == camelWave {
		return
	}
	count := 1 + (g.wave-bunkerRunWave)/3
	if count > 3 {
		count = 3
	}
	// spawnInterval at 30 Hz — see updateCombat's spawn-pacing.
	spawnInterval := 120 - g.wave*10
	spawnInterval = spawnInterval / 2
	if spawnInterval < 20 {
		spawnInterval = 20
	}
	approxWaveFrames := (2+g.wave*2)*spawnInterval + 150 // index.html had +300 at 60Hz
	var fracs []float64
	switch count {
	case 1:
		fracs = []float64{0.5}
	case 2:
		fracs = []float64{0.33, 0.72}
	default:
		fracs = []float64{0.22, 0.53, 0.82}
	}
	for _, f := range fracs {
		jitter := g.rng.Intn(120) - 60 // halved from 60Hz [-60, 60] frames
		at := int(float64(approxWaveFrames)*f) + jitter
		if at < 60 {
			at = 60
		}
		g.bunkerSchedule = append(g.bunkerSchedule, at)
	}
}

// ─── Launcher slot / ownership ─────────────────────────────

// launcherSlotX — deterministic X position for a reserved slot. 8 slots evenly
// spaced across [120, worldW-120] so launchers never overlap and a returning
// player lands at the same X. Schema doc §"Shared state model".
//
// With worldW=800 and 8 slots:
//   slot 0 = 120, 1 = 200, 2 = 280, 3 = 360,
//   slot 4 = 440, 5 = 520, 6 = 600, 7 = 680
func launcherSlotX(slot int) float64 {
	if maxLauncherSlots <= 1 {
		return float64(worldW) / 2
	}
	const margin = 120.0
	span := float64(worldW) - 2*margin
	return margin + span*float64(slot)/float64(maxLauncherSlots-1)
}

// findFreeSlot — returns the lowest-indexed slot not currently held by a live
// launcher in g.launchers. Returns (-1, false) if all 8 slots are taken
// (caller should refuse the join; ws.go's maxPlayers cap prevents this case
// in practice).
func (g *gameState) findFreeSlot() (int, bool) {
	taken := [maxLauncherSlots]bool{}
	for _, L := range g.launchers {
		if L.Slot >= 0 && L.Slot < maxLauncherSlots {
			taken[L.Slot] = true
		}
	}
	for i := 0; i < maxLauncherSlots; i++ {
		if !taken[i] {
			return i, true
		}
	}
	return -1, false
}

// spawnLauncherForOwner — spawn a fresh launcher owned by ownerID at the
// first free slot. Returns (launcher_idx, slot, true) on success; on slot
// exhaustion returns (-1, -1, false). The launcher starts at full ammo with
// fire_mode=0, exactly mirroring the post-reset state of single-player's
// first launcher.
func (g *gameState) spawnLauncherForOwner(ownerID uint32) (int, int, bool) {
	slot, ok := g.findFreeSlot()
	if !ok {
		return -1, -1, false
	}
	// Initial ammo budget mirrors startCombat's per-launcher target. We
	// pick a generous default for any pre-COMBAT spawn; startCombat will
	// re-allocate on the next COMBAT entry anyway.
	const initialAmmo = 8
	L := &launcher{
		X:                launcherSlotX(slot),
		InterceptorsLeft: initialAmmo,
		FireMode:         0,
		OwnerPlayerID:    ownerID,
		Slot:             slot,
	}
	g.launchers = append(g.launchers, L)
	return len(g.launchers) - 1, slot, true
}

// despawnLauncherForOwner — remove the launcher belonging to ownerID. Returns
// true if a launcher was removed. Any in-flight interceptors with this owner
// stay alive (they carry their own OwnerPlayerID and don't depend on the
// launcher's continued existence).
func (g *gameState) despawnLauncherForOwner(ownerID uint32) bool {
	if ownerID == 0 {
		return false
	}
	for i, L := range g.launchers {
		if L.OwnerPlayerID == ownerID {
			g.launchers = append(g.launchers[:i], g.launchers[i+1:]...)
			return true
		}
	}
	return false
}

// launcherIdxForOwner — returns the index in g.launchers of the launcher
// owned by ownerID. Returns -1 if the owner doesn't currently have one (e.g.
// pre-spawn race during a join, or post-despawn). Used by the owner-check
// in input dispatch.
func (g *gameState) launcherIdxForOwner(ownerID uint32) int {
	if ownerID == 0 {
		return -1
	}
	for i, L := range g.launchers {
		if L.OwnerPlayerID == ownerID {
			return i
		}
	}
	return -1
}

// ─── Launcher ops ──────────────────────────────────────────

func (g *gameState) setFireMode(idx int, mode int) {
	if idx < 0 || idx >= len(g.launchers) || mode < 0 || mode > 2 {
		return
	}
	L := g.launchers[idx]
	L.FireMode = mode
	if mode == 0 {
		L.Arming = false
		L.ArmTimer = 0
	}
}

func (g *gameState) triggerResupply(idx int) {
	if idx < 0 || idx >= len(g.launchers) {
		return
	}
	L := g.launchers[idx]
	if L.Resupplying || L.Arming {
		return
	}
	L.Resupplying = true
	L.ResupplyTimer = 0
}

// moveLauncher — input.launcher_move. dx clamped to ±launcherMoveMaxDX,
// resulting X clamped to [40, worldW-40]. Returns false on a bad launcher_idx
// so the caller can record the drop metric.
func (g *gameState) moveLauncher(idx int, dx float64) bool {
	if idx < 0 || idx >= len(g.launchers) {
		return false
	}
	if dx > launcherMoveMaxDX {
		dx = launcherMoveMaxDX
	}
	if dx < -launcherMoveMaxDX {
		dx = -launcherMoveMaxDX
	}
	L := g.launchers[idx]
	L.X += dx
	if L.X < 40 {
		L.X = 40
	}
	if L.X > worldW-40 {
		L.X = worldW - 40
	}
	return true
}

// startBunkerRun — input.bunker_run. Index.html L685-689. Idempotent: ignored
// unless bunker is in active phase AND the run hasn't already been committed.
func (g *gameState) startBunkerRun() {
	if g.bunkerPhase != "active" {
		return
	}
	if g.bunkerRunTimer > 0 {
		return
	}
	g.bunkerRunTimer = 1
}

// releaseCamel — input.camel_release. Index.html L598-602. Pops from the
// shared camelPool and spawns a camel at a random edge.
func (g *gameState) releaseCamel() {
	if !g.camelMode || g.camelPool <= 0 {
		return
	}
	g.camels = append(g.camels, g.makeCamel(g.rng.Float64() < 0.5))
	g.camelPool--
}

func (g *gameState) makeCamel(fromLeft bool) *camel {
	var x, vx float64
	dir := 1
	if fromLeft {
		x = -20
		vx = 0.8 + g.rng.Float64()*0.5
		dir = 1
	} else {
		x = float64(worldW) + 20
		vx = -(0.8 + g.rng.Float64()*0.5)
		dir = -1
	}
	return &camel{
		EID:          g.nextID(),
		X:            x,
		VX:           vx,
		Dir:          dir,
		Bob:          g.rng.Float64() * math.Pi * 2,
		SpitCooldown: 15 + g.rng.Intn(15), // halved from 30-60 frames at 60Hz
		Alive:        true,
	}
}

// fireFromLauncher — port of index.html's fireFromLauncher. Returns true if a
// fire was *initiated* (manual = interceptor spawned; arming = arm timer
// started). Caller uses the return to gate the per-player spaceCooldown.
func (g *gameState) fireFromLauncher(idx int, p *Player) bool {
	if g.phase != phaseCombat {
		return false
	}
	if idx < 0 || idx >= len(g.launchers) {
		return false
	}
	if g.bunkerLocksFire() {
		return false
	}
	L := g.launchers[idx]
	if L.Arming || L.Resupplying {
		return false
	}
	if L.FireMode == 1 || L.FireMode == 2 {
		if L.InterceptorsLeft > 0 {
			L.Arming = true
			L.ArmTimer = 0
			// Remember which player kicked off the arming so the eventual
			// launchInterceptor on arm-timer expiry attributes ownership
			// correctly. (Pre-per-player-ownership the armed branch always
			// reported owner=0; with one launcher per player this is now
			// equal to L.OwnerPlayerID, but recording it explicitly keeps
			// the contract clear.)
			if p != nil {
				L.ArmingByPlayerID = p.ID
			} else {
				L.ArmingByPlayerID = L.OwnerPlayerID
			}
			// We charge the player a shot on the spawn (in updateArm), not here.
			return true
		}
		return false
	}
	// Manual — spawn immediately.
	g.launchInterceptor(idx, false, false, p)
	return true
}

// launchInterceptor — creates the interceptor entity and emits missile.fired.
// Convenience wrapper around launchInterceptorAs that derives the owner ID
// from a *Player (caller may pass nil for an unowned spawn).
func (g *gameState) launchInterceptor(idx int, armed, autoPilot bool, p *Player) {
	var ownerID uint32
	if p != nil {
		ownerID = p.ID
	}
	g.launchInterceptorAs(idx, armed, autoPilot, ownerID)
}

// launchInterceptorAs — spawn an interceptor attributed to ownerID. Called
// directly from the arm-timer expiry path (which knows the owning player by
// ID, not by *Player struct).
func (g *gameState) launchInterceptorAs(idx int, armed, autoPilot bool, ownerID uint32) {
	L := g.launchers[idx]
	if L.InterceptorsLeft <= 0 {
		return
	}
	L.InterceptorsLeft--
	ic := &interceptor{
		EID:           g.nextID(),
		X:             L.X,
		Y:             worldH - 60,
		VX:            0,
		VY:            -3.5,
		Alive:         true,
		Armed:         armed,
		AutoPilot:     autoPilot,
		OwnerPlayerID: ownerID,
		LauncherIdx:   idx,
		Path:          &pathRing{},
	}
	g.interceptors = append(g.interceptors, ic)
	// Emit missile.fired.
	mode := 0
	if armed && autoPilot {
		mode = 2
	} else if armed {
		mode = 1
	}
	g.publish(EventMissileFired, map[string]any{
		"interceptor_eid": ic.EID,
		"launcher_idx":    idx,
		"launcher_pos":    map[string]float64{"x": L.X, "y": worldH - 60},
		"player_id":       ownerID,
		"fire_mode":       mode,
		"wave":            g.wave,
	})
}

// steerInterceptor — input.steer. Owner-only.
func (g *gameState) steerInterceptor(eid uint32, aim *Vec2, requester uint32) {
	if aim == nil {
		return
	}
	for _, ic := range g.interceptors {
		if ic.EID != eid {
			continue
		}
		if ic.OwnerPlayerID != requester {
			droppedInputsInc("not_owner")
			return
		}
		ic.AimX = aim.X
		ic.AimY = aim.Y
		ic.HasAim = true
		return
	}
}

// placeRadar — EMPLACEMENT-only. Clamps to (40..760, 100..580).
func (g *gameState) placeRadar(idx int, pos *Vec2) {
	if pos == nil || idx < 0 || idx >= len(g.radars) {
		return
	}
	x := pos.X
	y := pos.Y
	if x < 40 {
		x = 40
	}
	if x > worldW-40 {
		x = worldW - 40
	}
	if y < 100 {
		y = 100
	}
	if y > worldH-120 {
		y = worldH - 120
	}
	g.radars[idx].X = x
	g.radars[idx].Y = y
}

// ─── Tick ──────────────────────────────────────────────────

func (g *gameState) tick(tickNum uint32) {
	// Age flash counters always.
	if g.killFlash > 0 {
		g.killFlash--
	}
	if g.lifeUpFlash > 0 {
		g.lifeUpFlash--
	}
	if g.phase == phaseWaveEnd {
		g.waveEndTimer++
	}
	if g.phase == phaseCelebration {
		// During CELEBRATION the physics is frozen; only the celebration timer
		// decrements. When it hits zero, fall through to GAME_OVER.
		if g.celebrationTimer > 0 {
			g.celebrationTimer--
		}
		if g.celebrationTimer <= 0 {
			g.phase = phaseGameOver
		}
		// Don't age ephemerals — keep the final frame stable for the confetti
		// overlay. (Matches index.html L2580 — game state is paused.)
		return
	}
	if g.phase != phaseCombat {
		// Even outside combat, age ephemeral effects so they don't linger.
		g.ageEphemerals()
		return
	}
	g.updateCombat()
	g.updateBunker()
	g.updateCamels()
	g.ageEphemerals()
	// Wave-end detection runs after the per-system updates so a leak inside
	// the bunker-resolution path can still flip lives→0.
	g.checkWaveEnd()
}

func (g *gameState) ageEphemerals() {
	// Explosions live ~50 ticks (~1.66s); blast waves ~33 ticks (~1s).
	const expLifetime = 50
	const bwLifetime = 33
	w := g.explosions[:0]
	for _, e := range g.explosions {
		e.Age++
		if e.Age < expLifetime {
			w = append(w, e)
		}
	}
	g.explosions = w
	bw := g.blastWaves[:0]
	for _, b := range g.blastWaves {
		b.Radius += (b.MaxRadius - b.Radius) * 0.15
		if b.Radius < b.MaxRadius-1 {
			bw = append(bw, b)
		}
	}
	g.blastWaves = bw
}

func (g *gameState) updateCombat() {
	g.waveTimer++

	// Arm timers — per launcher.
	for li, L := range g.launchers {
		if L.Arming {
			L.ArmTimer++
			req := armTime
			if L.FireMode == 2 {
				req = autopilotArmTime
			}
			if L.ArmTimer >= req {
				L.Arming = false
				L.ArmTimer = 0
				// Owner attribution: prefer the player who initiated arming
				// (L.ArmingByPlayerID); fall back to the launcher's owner
				// for the (impossible-in-practice) case where arming was
				// initiated with no recorded player. Clear after use.
				ownerID := L.ArmingByPlayerID
				if ownerID == 0 {
					ownerID = L.OwnerPlayerID
				}
				L.ArmingByPlayerID = 0
				g.launchInterceptorAs(li, true, L.FireMode == 2, ownerID)
			}
		}
	}
	// Resupply timers — per launcher.
	for _, L := range g.launchers {
		if L.Resupplying {
			L.ResupplyTimer++
			if L.ResupplyTimer >= resupplyTime {
				L.Resupplying = false
				L.ResupplyTimer = 0
				L.InterceptorsLeft += 3 + g.wave/2
			}
		}
	}

	// Spawn shaheds.
	g.spawnTimer++
	spawnInterval := 120 - g.wave*10
	// Halve because we run at 30 Hz, not 60 Hz.
	spawnInterval = spawnInterval / 2
	if spawnInterval < 20 {
		spawnInterval = 20
	}
	if g.shahedsToSpawn > 0 && g.spawnTimer >= spawnInterval {
		isCruise := g.rng.Float64() < 0.2
		g.shaheds = append(g.shaheds, g.makeShahed(isCruise))
		g.shahedsToSpawn--
		g.spawnTimer = 0
	}

	// Update shaheds.
	for _, s := range g.shaheds {
		if !s.Alive {
			continue
		}
		g.updateShahed(s)
		if s.Y >= float64(groundY) {
			s.Alive = false
			g.logShahedPath(s, "impact")
			loss := 1
			if s.Cruise {
				loss = 2
			}
			g.lives -= loss
			if g.lives < 0 {
				g.lives = 0
			}
			g.leakedShaheds++
			g.explosions = append(g.explosions, &explosion{X: s.X, Y: float64(groundY), Color: "orange"})
			// shahed.impact event
			g.publish(EventShahedImpact, map[string]any{
				"shahed_eid":      s.EID,
				"impact_pos":      map[string]float64{"x": s.X, "y": float64(groundY)},
				"lives_remaining": g.lives,
				"wave":            g.wave,
			})
		}
	}

	// Update interceptors.
	for _, in := range g.interceptors {
		if !in.Alive {
			continue
		}
		in.Age++
		// Autopilot: seek nearest threat within detection radius.
		if in.AutoPilot {
			var nearest *shahed
			nearestDist := math.Inf(1)
			for _, s := range g.shaheds {
				if !s.Alive {
					continue
				}
				d := math.Hypot(in.X-s.X, in.Y-s.Y)
				if d < autopilotDetect && d < nearestDist {
					nearest = s
					nearestDist = d
				}
			}
			if nearest != nil {
				dx := nearest.X - in.X
				dy := nearest.Y - in.Y
				ang := math.Atan2(dy, dx)
				speed := math.Hypot(in.VX, in.VY)
				const turnRate = 0.08
				cur := math.Atan2(in.VY, in.VX)
				diff := ang - cur
				for diff > math.Pi {
					diff -= 2 * math.Pi
				}
				for diff < -math.Pi {
					diff += 2 * math.Pi
				}
				step := turnRate
				if math.Abs(diff) < step {
					step = math.Abs(diff)
				}
				newAng := cur + math.Copysign(step, diff)
				in.VX = math.Cos(newAng) * speed
				in.VY = math.Sin(newAng) * speed
			}
		}
		// Manual steering (input.steer aim point — owner already gated).
		if in.HasAim {
			dx := in.AimX - in.X
			dy := in.AimY - in.Y
			dist := math.Hypot(dx, dy)
			const deadzone = 15.0
			if dist > deadzone {
				force := math.Min(dist/120.0, 1.0) * 0.35
				in.VX += (dx / dist) * force
				in.VY += (dy / dist) * force
			}
		}
		// Drag + bias.
		in.VX *= 0.96
		if in.VY < 0 {
			in.VY -= 0.02
		} else {
			in.VY += 0.02
		}
		if in.VY < -6 {
			in.VY = -6
		}
		if in.VY > 3 {
			in.VY = 3
		}
		in.X += in.VX
		in.Y += in.VY
		// Off-screen check.
		if in.Y < -20 || in.Y > float64(groundY)+5 || in.X < -20 || in.X > worldW+20 {
			in.Alive = false
			g.logInterceptorPath(in, "miss")
			continue
		}
		// Record path sample (same every-other-tick cadence as shaheds).
		if in.Path != nil && in.Age%2 == 0 {
			in.Path.push(in.X, in.Y)
			in.FullPath = append(in.FullPath, pathPoint{X: in.X, Y: in.Y})
		}
		// Collision.
		if in.Armed {
			// Proximity fuse.
			var triggered bool
			for _, s := range g.shaheds {
				if !s.Alive {
					continue
				}
				if math.Hypot(in.X-s.X, in.Y-s.Y) < proxTrigger {
					triggered = true
					break
				}
			}
			if triggered {
				g.blastWaves = append(g.blastWaves, &blastWave{X: in.X, Y: in.Y, MaxRadius: proxRadius})
				kills := 0
				for _, s := range g.shaheds {
					if !s.Alive {
						continue
					}
					if math.Hypot(in.X-s.X, in.Y-s.Y) < proxRadius {
						s.Alive = false
						g.logShahedPath(s, "intercepted")
						kills++
						kx := s.X
						ky := s.Y
						if s.Cruise {
							g.score += 250
						} else {
							g.score += 100
						}
						g.explosions = append(g.explosions, &explosion{X: kx, Y: ky, Color: "green"})
						// In-radar bonus
						for _, r := range g.radars {
							if math.Hypot(kx-r.X, ky-r.Y) < r.Radius {
								g.score += 50
								break
							}
						}
						// missile.intercept event — one per kill
						g.publish(EventMissileIntercept, map[string]any{
							"interceptor_eid": in.EID,
							"shahed_eid":      s.EID,
							"kill_pos":        map[string]float64{"x": kx, "y": ky},
							"player_id":       in.OwnerPlayerID,
							"wave":            g.wave,
						})
						g.attributeKill(in.OwnerPlayerID, 1)
						g.recordKill(s, kx, ky, in.OwnerPlayerID)
					}
				}
				if kills > 1 {
					g.score += int64(kills) * 75
				}
				in.Alive = false
				g.logInterceptorPath(in, "hit")
				g.explosions = append(g.explosions, &explosion{X: in.X, Y: in.Y, Color: "green"})
				if kills > 1 {
					g.killFlash = 45 // ~1.5s at 30 Hz
				} else if kills == 1 {
					g.killFlash = 30
				}
			}
		} else {
			// Direct contact — kinetic.
			for _, s := range g.shaheds {
				if !s.Alive {
					continue
				}
				if math.Hypot(in.X-s.X, in.Y-s.Y) < 22 {
					s.Alive = false
					in.Alive = false
					g.logShahedPath(s, "intercepted")
					g.logInterceptorPath(in, "hit")
					if s.Cruise {
						g.score += 250
					} else {
						g.score += 100
					}
					for _, r := range g.radars {
						if math.Hypot(s.X-r.X, s.Y-r.Y) < r.Radius {
							g.score += 50
							break
						}
					}
					g.explosions = append(g.explosions, &explosion{X: s.X, Y: s.Y, Color: "green"})
					g.killFlash = 30
					g.publish(EventMissileIntercept, map[string]any{
						"interceptor_eid": in.EID,
						"shahed_eid":      s.EID,
						"kill_pos":        map[string]float64{"x": s.X, "y": s.Y},
						"player_id":       in.OwnerPlayerID,
						"wave":            g.wave,
					})
					g.attributeKill(in.OwnerPlayerID, 1)
					g.recordKill(s, s.X, s.Y, in.OwnerPlayerID)
					break
				}
			}
		}
	}

}

// checkWaveEnd — pulled out of updateCombat so updateBunker can flip lives→0
// (bunker hit) and still trigger the same wave-end path. Called once per tick
// during COMBAT, after all per-system updates.
func (g *gameState) checkWaveEnd() {
	if g.phase != phaseCombat {
		return
	}
	aliveShaheds := 0
	for _, s := range g.shaheds {
		if s.Alive {
			aliveShaheds++
		}
	}
	if g.lives <= 0 {
		g.emitWaveSummaryAndReset()
		g.phase = phaseGameOver
		// Reset per-entity paths on transition out of COMBAT.
		g.resetEntityPaths()
		return
	}
	if g.shahedsToSpawn == 0 && aliveShaheds == 0 {
		g.emitWaveSummaryAndReset()
		g.phase = phaseWaveEnd
		g.score += int64(g.wave) * 50
		g.waveEndTimer = 0
		g.resetEntityPaths()
	}
}

// resetEntityPaths — clear all per-entity ring buffers. Called on transitions
// out of COMBAT so EMPLACEMENT-time radar drag doesn't pollute trajectories.
func (g *gameState) resetEntityPaths() {
	for _, s := range g.shaheds {
		if s.Path != nil {
			s.Path.reset()
		}
	}
	for _, in := range g.interceptors {
		if in.Path != nil {
			in.Path.reset()
		}
	}
}

// emitWaveSummaryAndReset — flush the per-wave kill collector to the Room
// callback (if any) and reset for the next wave.
func (g *gameState) emitWaveSummaryAndReset() {
	if g.onWaveSummary != nil && (len(g.killsThisWave) > 0 || g.leakedShaheds > 0 ||
		len(g.wavePaths.Shaheds) > 0 || len(g.wavePaths.Interceptors) > 0) {
		// Pass copies so the Room's broadcast goroutine has stable data.
		copyKills := make([]killSnapshot, len(g.killsThisWave))
		copy(copyKills, g.killsThisWave)
		copyShahedPaths := make([]pathRecord, len(g.wavePaths.Shaheds))
		copy(copyShahedPaths, g.wavePaths.Shaheds)
		copyInterceptorPaths := make([]pathRecord, len(g.wavePaths.Interceptors))
		copy(copyInterceptorPaths, g.wavePaths.Interceptors)
		g.onWaveSummary(g.wave, copyKills, g.leakedShaheds, copyShahedPaths, copyInterceptorPaths)
	}
	g.killsThisWave = g.killsThisWave[:0]
	g.leakedShaheds = 0
	// wavePaths NOT reset here — it persists across the COMBAT→WAVE_END→GAME_OVER
	// transitions so the GAME_OVER wave.summary still carries them. Cleared on
	// next startCombat().
}

// makeShahed — port of index.html L884-908. Cruise variants use ~3x speed +
// reduced amplitude/wobble; the straight-line cruise behavior comes from the
// pattern dispatcher in updateShahed.
func (g *gameState) makeShahed(cruise bool) *shahed {
	pattern := g.rng.Intn(7)
	startX := 80 + g.rng.Float64()*(worldW-160)
	baseSpeed := 0.6 + float64(g.wave)*0.12 + g.rng.Float64()*0.3
	speed := baseSpeed
	if cruise {
		speed = baseSpeed * 3
	}
	amplitude := 40 + g.rng.Float64()*60
	if cruise {
		amplitude = 25 + g.rng.Float64()*35
	}
	freq := 0.008 + g.rng.Float64()*0.012
	phaseOff := g.rng.Float64() * math.Pi * 2
	wobble := 0.3 + g.rng.Float64()*0.5
	if cruise {
		wobble = 0.1 + g.rng.Float64()*0.2
	}
	return &shahed{
		EID:       g.nextID(),
		X:         startX,
		Y:         -30,
		BaseX:     startX,
		PrevX:     startX,
		Speed:     speed,
		Pattern:   pattern,
		Amplitude: amplitude,
		Frequency: freq,
		PhaseOff:  phaseOff,
		Wobble:    wobble,
		Cruise:    cruise,
		Alive:     true,
		Path:      &pathRing{},
	}
}

// updateShahed — ported from index.html L961-1041, including all 7 patterns
// (L974-1006) and the cruise straight-line case (L884-908 — cruise = ×3 speed
// + minimal lateral). Halved time scaling: t increments at 30 Hz, so we
// double tEq when feeding it into 60 Hz-tuned sinusoid frequencies.
func (g *gameState) updateShahed(s *shahed) {
	s.T++
	prevX := s.X
	prevVisible := s.Visible
	// Retreating — index.html L963-971. A camel spit flipped this shahed.
	// Shahed flies up and off, no further pattern logic.
	if s.Retreating {
		s.RetreatAccel += 0.025
		s.Y -= 2.5 + s.RetreatAccel
		s.X += s.RetreatVX
		if s.T%2 == 0 && s.Path != nil {
			s.Path.push(s.X, s.Y)
			s.FullPath = append(s.FullPath, pathPoint{X: s.X, Y: s.Y})
		}
		if s.Y < -40 {
			s.Alive = false
			g.logShahedPath(s, "retreated")
		}
		s.Visible = 0.9
		s.VX = s.X - prevX
		s.VY = -2.5 - s.RetreatAccel
		return
	}
	t := float64(s.T)
	// At 30 Hz, advance by 2× per tick to match 60 Hz wall-clock pace.
	tEq := t * 2.0
	// Cruise variants — index.html L884-908: high-speed straight-line with
	// minimal sinusoid. The flag is decisive; pattern dispatch is skipped.
	if s.Cruise {
		// Light sinusoid only — amplitude is ~1/3 of normal already; using the
		// baseline pattern preserves the "fast, mostly straight" feel.
		s.X = s.BaseX + math.Sin(tEq*s.Frequency+s.PhaseOff)*s.Amplitude
	} else {
		switch s.Pattern {
		case 0:
			// Sinusoidal
			s.X = s.BaseX + math.Sin(tEq*s.Frequency+s.PhaseOff)*s.Amplitude
		case 1:
			// Zigzag
			period := 120.0 / s.Frequency
			zigPhase := math.Mod(tEq+s.PhaseOff*100, period) / period
			var delta float64
			if zigPhase < 0.5 {
				delta = zigPhase*2 - 0.5
			} else {
				delta = 1.5 - zigPhase*2
			}
			s.X = s.BaseX + delta*s.Amplitude
		case 2:
			// Spiral/corkscrew approach
			ramp := math.Min(1, tEq/200)
			s.X = s.BaseX + math.Sin(tEq*s.Frequency*1.5+s.PhaseOff)*s.Amplitude*ramp
		case 3:
			// Jinking — random direction changes. Index.html L986-991.
			// Original uses a per-frame modulo trigger with random period;
			// at 30 Hz we halve the periods so wall-clock jink frequency matches.
			jinkPeriod := 25 + g.rng.Intn(15) // halved from 50 + rng(30)
			if s.T%jinkPeriod < 1 {
				s.BaseX += (g.rng.Float64() - 0.5) * s.Amplitude * 0.6
				if s.BaseX < 40 {
					s.BaseX = 40
				}
				if s.BaseX > worldW-40 {
					s.BaseX = worldW - 40
				}
			}
			s.X += (s.BaseX - s.X) * 0.03
		case 4:
			// Straight down — no lateral movement. Index.html L993-995.
			s.X = s.BaseX
		case 5:
			// Diagonal drift — set drift once, apply linearly. Index.html L996-999.
			if s.Drift == 0 {
				dir := 1.0
				if g.rng.Float64() < 0.5 {
					dir = -1.0
				}
				s.Drift = dir * (0.3 + g.rng.Float64()*0.5)
			}
			s.X = s.BaseX + s.Drift*tEq
		case 6:
			// Diving S-curve — one big S then straight. Index.html L1000-1005.
			// At 60Hz the S lasts t<200; at 30Hz that's t<100 (since we use tEq).
			if tEq < 200 {
				s.X = s.BaseX + math.Sin(tEq*0.015+s.PhaseOff)*s.Amplitude*1.5
			}
			// else: holds last s.X (goes straight)
		default:
			// Defensive fallback — patterns are randomly assigned from intn(7),
			// so this branch should be unreachable.
			s.X = s.BaseX + math.Sin(tEq*s.Frequency+s.PhaseOff)*s.Amplitude
		}
	}
	s.X += math.Sin(tEq*0.07+s.PhaseOff*3) * s.Wobble * 3
	// At 30 Hz with half the frames per second, double per-frame displacement
	// to cover the same wall-clock distance as 60 Hz.
	s.Y += s.Speed * 2
	s.VX = s.X - prevX
	s.VY = s.Speed * 2
	s.PrevX = prevX
	if s.X < 20 {
		s.X = 20
	}
	if s.X > worldW-20 {
		s.X = worldW - 20
	}
	// Visibility — max over all radars (in-circle only).
	var maxVis float64
	for _, r := range g.radars {
		d := math.Hypot(s.X-r.X, s.Y-r.Y)
		if d < r.Radius {
			v := 1 - (d/r.Radius)*0.3
			if v > maxVis {
				maxVis = v
			}
		}
	}
	// Smooth toward maxVis (same 10% IIR as 60Hz; visually OK).
	s.Visible += (maxVis - s.Visible) * 0.1
	if maxVis == 0 {
		s.Visible *= 0.93
	}
	// radar.detection event on the 0→positive transition.
	if prevVisible <= 0.001 && s.Visible > 0.001 {
		// Find which radar tracked first (any radar in range).
		var trackedIdx int
		var trackedPos map[string]float64
		for i, r := range g.radars {
			if math.Hypot(s.X-r.X, s.Y-r.Y) < r.Radius {
				trackedIdx = i
				trackedPos = map[string]float64{"x": r.X, "y": r.Y}
				break
			}
		}
		g.publish(EventRadarDetection, map[string]any{
			"shahed_eid":    s.EID,
			"shahed_pos":    map[string]float64{"x": s.X, "y": s.Y},
			"shahed_vel":    map[string]float64{"x": s.VX, "y": s.VY},
			"shahed_cruise": s.Cruise,
			"radar_idx":     trackedIdx,
			"radar_pos":     trackedPos,
			"wave":          g.wave,
		})
	}
	// Record path sample. At 30 Hz, sampling every other tick still gives ~15
	// samples / second = a half-second of history per ring. Matches index.html
	// L1006 "every 3 frames" at 60 Hz wall-clock.
	if s.Path != nil && s.T%2 == 0 {
		s.Path.push(s.X, s.Y)
		s.FullPath = append(s.FullPath, pathPoint{X: s.X, Y: s.Y})
	}
}

// logShahedPath — snapshots a shahed's trajectory + outcome into wavePaths
// at the moment of death. Called from each shahed-death site so the WAVE_END
// review screen can render all paths color-coded by outcome (matches solo's
// trajectories.shaheds map at index.html L720, L2080-L2092).
func (g *gameState) logShahedPath(s *shahed, outcome string) {
	// Snapshot FullPath (unbounded across the entity's lifetime) — not the
	// ring buffer, which only retains the last ~2 seconds of history. The
	// wave-end review needs the entire flight from spawn to demise.
	var p []pathPoint
	if len(s.FullPath) > 0 {
		p = make([]pathPoint, len(s.FullPath))
		copy(p, s.FullPath)
	} else if s.Path != nil {
		p = s.Path.snapshot()
	}
	g.wavePaths.Shaheds = append(g.wavePaths.Shaheds, pathRecord{Path: p, Outcome: outcome})
}

// logInterceptorPath — same shape, for interceptor deaths.
func (g *gameState) logInterceptorPath(in *interceptor, outcome string) {
	var p []pathPoint
	if len(in.FullPath) > 0 {
		p = make([]pathPoint, len(in.FullPath))
		copy(p, in.FullPath)
	} else if in.Path != nil {
		p = in.Path.snapshot()
	}
	g.wavePaths.Interceptors = append(g.wavePaths.Interceptors, pathRecord{Path: p, Outcome: outcome})
}

// recordKill — collects a kill snapshot for the per-wave wave.summary frame.
// Captures the killed shahed's current path[] so the renderer can draw the
// trajectory dotted line. Index.html L867-878 + L2013/L2046.
func (g *gameState) recordKill(s *shahed, killX, killY float64, ownerID uint32) {
	// Capture FullPath (unbounded trail across the entity's lifetime) — not
	// the ring buffer, which only retains the last ~2 seconds. Matches the
	// behaviour of logShahedPath; the kill-marker freeze-frame trajectory is
	// drawn from this same path.
	var p []pathPoint
	if len(s.FullPath) > 0 {
		p = make([]pathPoint, len(s.FullPath))
		copy(p, s.FullPath)
	} else if s.Path != nil {
		p = s.Path.snapshot()
	}
	// Capture all OTHER alive shaheds at this instant. Bound the size to keep
	// the wave.summary frame within the 16 KiB WS read limit even on busy
	// waves (~20 kills × ~10 shaheds × ~80 bytes JSON ≈ 16 KiB).
	world := make([]shahedSnap, 0, len(g.shaheds))
	for _, other := range g.shaheds {
		if !other.Alive || other == s {
			continue
		}
		world = append(world, shahedSnap{
			EID:    other.EID,
			X:      other.X,
			Y:      other.Y,
			VX:     other.VX,
			VY:     other.VY,
			Cruise: other.Cruise,
		})
	}
	g.killsThisWave = append(g.killsThisWave, killSnapshot{
		ShahedEID:    s.EID,
		KillX:        killX,
		KillY:        killY,
		PlayerID:     ownerID,
		Tick:         uint32(g.waveTimer),
		Path:         p,
		WorldShaheds: world,
	})
}

// updateBunker — index.html L691-731 ported, 30 Hz timer constants.
// Lifecycle: idle (waiting for scheduled trigger) → active (warning + optional
// RUN commit) → post (resolve safe/hit; hit drops lives to 0). The active→post
// outcome is `safe` only if the run was committed early enough that the
// character reached the bunker before the warning ended.
func (g *gameState) updateBunker() {
	switch g.bunkerPhase {
	case "idle":
		if len(g.bunkerSchedule) == 0 {
			return
		}
		if g.bunkerDone < len(g.bunkerSchedule) && g.waveTimer >= g.bunkerSchedule[g.bunkerDone] {
			g.bunkerPhase = "active"
			g.bunkerTimer = bunkerWarningTicks
			g.bunkerRunTimer = 0
			g.bunkerOutcome = "pending"
		}
	case "active":
		g.bunkerTimer--
		if g.bunkerRunTimer > 0 {
			g.bunkerRunTimer++
		}
		if g.bunkerTimer <= 0 {
			// Outcome: safe iff the player committed RUN with enough time
			// remaining for the character to reach the bunker.
			if g.bunkerRunTimer >= bunkerRunTicks {
				g.bunkerOutcome = "safe"
			} else {
				g.bunkerOutcome = "hit"
			}
			g.bunkerPhase = "post"
			g.bunkerPostTimer = bunkerPostTicks
		}
	case "post":
		g.bunkerPostTimer--
		if g.bunkerPostTimer <= 0 {
			if g.bunkerOutcome == "hit" {
				// Bunker hit — index.html L714-722 sets lives = 0. checkWaveEnd
				// catches that on the same tick and flips to GAME_OVER.
				g.lives = 0
				g.bunkerPhase = "idle"
				return
			}
			g.bunkerPhase = "idle"
			g.bunkerDone++
			g.bunkerRunTimer = 0
			g.bunkerOutcome = "pending"
		}
	}
}

// bunkerLocksFire — index.html L733-735. Fire is locked once the player has
// committed to RUN (so they can't fire while running) and during the post-
// resolution phase. Caller: fireFromLauncher and friends use this gate.
func (g *gameState) bunkerLocksFire() bool {
	return (g.bunkerPhase == "active" && g.bunkerRunTimer > 0) || g.bunkerPhase == "post"
}

// updateCamels — index.html L603-660 ported, 30 Hz friendly. Camels walk the
// ground line tracking the nearest threatening shahed, spit periodically; spit
// hits flip the shahed into retreat (no life loss, +50 score).
func (g *gameState) updateCamels() {
	if !g.camelMode {
		return
	}
	// Auto-release one camel periodically as long as the pool has any left.
	if g.camelPool > 0 {
		g.camelReleaseTimer--
		if g.camelReleaseTimer <= 0 {
			g.releaseCamel()
			g.camelReleaseTimer = 75 // halved from 150 at 60Hz = 2.5s
		}
	}
	gy := float64(worldH) - 50 // ground line, matches index.html groundY
	// Move camels + spit if a threat is in range.
	for _, c := range g.camels {
		if !c.Alive {
			continue
		}
		c.Age++
		c.Bob += 0.5 // halved → wait, 0.25 at 60Hz means 0.5 at 30Hz to match wall-clock
		// Find nearest non-retreating shahed by horizontal distance.
		var target *shahed
		bestDx := math.Inf(1)
		for _, s := range g.shaheds {
			if !s.Alive || s.Retreating {
				continue
			}
			adx := math.Abs(s.X - c.X)
			if adx < bestDx {
				bestDx = adx
				target = s
			}
		}
		if target != nil {
			dx := target.X - c.X
			if math.Abs(dx) > 6 {
				dir := 1.0
				if dx < 0 {
					dir = -1.0
				}
				c.VX += dir * 0.16 // halved-frame friendly (0.08 × 2)
				if c.VX > 1.6 {
					c.VX = 1.6
				}
				if c.VX < -1.6 {
					c.VX = -1.6
				}
			} else {
				c.VX *= 0.55
			}
		}
		c.X += c.VX
		// Direction for renderer.
		if c.VX > 0.05 {
			c.Dir = 1
		} else if c.VX < -0.05 {
			c.Dir = -1
		}
		if c.X < 12 {
			c.X = 12
			c.VX = math.Abs(c.VX)
		}
		if c.X > worldW-12 {
			c.X = worldW - 12
			c.VX = -math.Abs(c.VX)
		}
		c.SpitCooldown--
		if c.SpitCooldown <= 0 && target != nil &&
			math.Abs(target.X-c.X) < camelSpitRangeX &&
			(gy-target.Y) < camelSpitRangeY {
			tdx := target.X - c.X
			tdy := target.Y - (gy - 14)
			dist := math.Hypot(tdx, tdy)
			if dist < 40 {
				dist = 40
			}
			speed := 5.5
			spawnX := c.X - 14
			if c.VX >= 0 {
				spawnX = c.X + 14
			}
			g.camelSpits = append(g.camelSpits, &camelSpit{
				EID:   g.nextID(),
				X:     spawnX,
				Y:     gy - 24,
				VX:    (tdx / dist) * speed,
				VY:    (tdy / dist) * speed,
				Alive: true,
			})
			c.SpitCooldown = camelSpitCooldown
		}
	}
	// Move spits, age them out, collide with shaheds.
	for _, p := range g.camelSpits {
		if !p.Alive {
			continue
		}
		p.X += p.VX
		p.Y += p.VY
		p.VY += 0.08 // halved from 60Hz (0.04 × 2)
		p.Age++
		if p.Age > 90 /* 180/2 */ || p.Y < -30 || p.Y > worldH || p.X < -20 || p.X > worldW+20 {
			p.Alive = false
			continue
		}
		for _, s := range g.shaheds {
			if !s.Alive || s.Retreating {
				continue
			}
			if math.Hypot(s.X-p.X, s.Y-p.Y) < 22 {
				s.Retreating = true
				s.RetreatVX = (g.rng.Float64() - 0.5) * 1.5
				s.RetreatAccel = 0
				p.Alive = false
				g.score += 50
				break
			}
		}
	}
	// Compact spit list (drop dead).
	w := g.camelSpits[:0]
	for _, p := range g.camelSpits {
		if p.Alive {
			w = append(w, p)
		}
	}
	g.camelSpits = w
	// Compact camel list (drop dead — currently camels never die organically
	// in the original; keep the compactor for forward-compat).
	cw := g.camels[:0]
	for _, c := range g.camels {
		if c.Alive {
			cw = append(cw, c)
		}
	}
	g.camels = cw
}

// attributeKill — bump room-wide intercept count (drives life-up threshold)
// and dispatch to the owner-player Intercepts counter via attributeFn (set
// by the Room in newRoom). attributeFn runs on the tick goroutine, so the
// player-map access is safe without a lock.
func (g *gameState) attributeKill(ownerID uint32, n int) {
	g.intercepts += n
	for g.intercepts >= g.lifeUpThreshold() {
		g.intercepts -= g.lifeUpThreshold()
		g.lives++
		g.lifeUpFlash = 60 // ~2s at 30 Hz
	}
	if g.attributeFn != nil {
		g.attributeFn(ownerID, n)
	}
}

// lifeUpThreshold — matches index.html L213.
func (g *gameState) lifeUpThreshold() int {
	t := int(math.Ceil(1.6 * float64(2+g.wave*2)))
	if t < 1 {
		t = 1
	}
	return t
}

// publish — fire-and-forget into the event bus.
func (g *gameState) publish(eventType string, payload map[string]any) {
	if g.bus == nil {
		return
	}
	g.bus.Publish(GameEvent{
		Type:      eventType,
		Timestamp: time.Now(),
		EventID:   newEventID(),
		Payload:   payload,
	})
}

// ─── Snapshot ──────────────────────────────────────────────

// snapshotMap — produce the state.tick payload (without the `type`/`tick`/`ts`
// fields, which the Room adds). Per design doc:
//   - per-entity path[] is included only during COMBAT (bandwidth).
//   - bunker block is null when bunkerPhase == "idle"; otherwise populated.
//   - camels[] and camel_spits[] are always emitted (empty slices in non-camel waves).
func (g *gameState) snapshotMap() map[string]any {
	combat := g.phase == phaseCombat
	shads := make([]map[string]any, 0, len(g.shaheds))
	for _, s := range g.shaheds {
		m := map[string]any{
			"eid":     s.EID,
			"pos":     map[string]float64{"x": s.X, "y": s.Y},
			"vel":     map[string]float64{"x": s.VX, "y": s.VY},
			"alive":   s.Alive,
			"cruise":  s.Cruise,
			"pattern": s.Pattern,
			"visible": s.Visible,
		}
		if combat && s.Path != nil && s.Path.n > 0 {
			m["path"] = pathToWire(s.Path)
		}
		shads = append(shads, m)
	}
	ints := make([]map[string]any, 0, len(g.interceptors))
	for _, in := range g.interceptors {
		m := map[string]any{
			"eid":             in.EID,
			"pos":             map[string]float64{"x": in.X, "y": in.Y},
			"vel":             map[string]float64{"x": in.VX, "y": in.VY},
			"alive":           in.Alive,
			"armed":           in.Armed,
			"auto_pilot":      in.AutoPilot,
			"age":             in.Age,
			"owner_player_id": in.OwnerPlayerID,
		}
		if combat && in.Path != nil && in.Path.n > 0 {
			m["path"] = pathToWire(in.Path)
		}
		ints = append(ints, m)
	}
	launchers := make([]map[string]any, 0, len(g.launchers))
	for i, L := range g.launchers {
		launchers = append(launchers, map[string]any{
			"idx":                i,
			"x":                  L.X,
			"interceptors_left":  L.InterceptorsLeft,
			"fire_mode":          L.FireMode,
			"arming":             L.Arming,
			"arm_timer":          L.ArmTimer,
			"resupplying":        L.Resupplying,
			"resupply_timer":     L.ResupplyTimer,
			// owner_player_id — per-player launcher ownership. Schema doc
			// §"WebSocket message schema" → state.tick.launchers[]. 0 only
			// during the brief pre-join race; once a player is joined, every
			// launcher in the snapshot has a non-zero owner.
			"owner_player_id":    L.OwnerPlayerID,
		})
	}
	radars := make([]map[string]any, 0, len(g.radars))
	for i, r := range g.radars {
		radars = append(radars, map[string]any{
			"idx":    i,
			"pos":    map[string]float64{"x": r.X, "y": r.Y},
			"radius": r.Radius,
		})
	}
	explosions := make([]map[string]any, 0, len(g.explosions))
	for _, e := range g.explosions {
		explosions = append(explosions, map[string]any{
			"x":     e.X,
			"y":     e.Y,
			"color": e.Color,
			"age":   e.Age,
		})
	}
	blastWaves := make([]map[string]any, 0, len(g.blastWaves))
	for _, b := range g.blastWaves {
		blastWaves = append(blastWaves, map[string]any{
			"x":          b.X,
			"y":          b.Y,
			"radius":     b.Radius,
			"max_radius": b.MaxRadius,
		})
	}
	// Camels + spits — only meaningful in camel mode but emit empty slices
	// otherwise so the renderer's array length checks behave consistently.
	camels := make([]map[string]any, 0, len(g.camels))
	for _, c := range g.camels {
		camels = append(camels, map[string]any{
			"eid":     c.EID,
			"pos":     map[string]float64{"x": c.X, "y": float64(worldH - 50)},
			"dir":     c.Dir,
			"walking": math.Abs(c.VX) > 0.05,
			"alive":   c.Alive,
		})
	}
	camelSpits := make([]map[string]any, 0, len(g.camelSpits))
	for _, p := range g.camelSpits {
		camelSpits = append(camelSpits, map[string]any{
			"eid":   p.EID,
			"pos":   map[string]float64{"x": p.X, "y": p.Y},
			"vel":   map[string]float64{"x": p.VX, "y": p.VY},
			"age":   p.Age,
			"alive": p.Alive,
		})
	}
	// Bunker — schema doc §state.tick.bunker. Null when phase=="idle".
	var bunker any
	if g.bunkerPhase != "idle" {
		var t int
		if g.bunkerPhase == "post" {
			t = g.bunkerPostTimer
		} else {
			t = g.bunkerTimer
		}
		bunker = map[string]any{
			"phase":     g.bunkerPhase,
			"timer":     t, // ticks at 30 Hz; clarified in design doc
			"run_timer": g.bunkerRunTimer,
			"outcome":   g.bunkerOutcome,
		}
	}
	return map[string]any{
		"phase":             string(g.phase),
		"wave":              g.wave,
		"lives":             g.lives,
		"score":             g.score,
		"intercepts":        g.intercepts,
		"life_up_threshold": g.lifeUpThreshold(),
		"wave_timer":        g.waveTimer,
		"wave_end_timer":    g.waveEndTimer,
		"space_cooldown":    0, // per-player; see schema doc
		"shaheds":           shads,
		"interceptors":      ints,
		"launchers":         launchers,
		"radars":            radars,
		"bunker":            bunker,
		"camel_mode":        g.camelMode,
		"camel_pool":        g.camelPool,
		"camels":            camels,
		"camel_spits":       camelSpits,
		"explosions":        explosions,
		"blast_waves":       blastWaves,
		// kill_flash + life_up_flash are emitted in 60-Hz-frame units so Agent 2's
		// renderer (which decrements at 60 fps) sees the original wall-clock
		// duration. Server ticks at 30 Hz so we multiply by 2 on the wire.
		"kill_flash":        g.killFlash * 2,
		"life_up_flash":     g.lifeUpFlash * 2,
	}
}

// pathToWire — convert a ring buffer to the on-wire [{x,y}, ...] shape.
// Called only when path is populated; we already gated on g.phase == phaseCombat.
func pathToWire(p *pathRing) []map[string]float64 {
	snap := p.snapshot()
	out := make([]map[string]float64, len(snap))
	for i, pp := range snap {
		out[i] = map[string]float64{"x": pp.X, "y": pp.Y}
	}
	return out
}
