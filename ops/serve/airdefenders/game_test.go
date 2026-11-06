// Game unit tests — focused on the new mechanics ported in the follow-on:
// camel system, bunker resolution, cruise kinematics, wave-pattern dispatcher,
// and the per-entity path[] ring buffer.
//
// All tests use a deterministic RNG so behaviour is reproducible across runs.
package main

import (
	"math"
	"math/rand"
	"testing"
)

// newDeterministicGame — produces a gameState whose RNG is seeded for stable test runs.
func newDeterministicGame(t *testing.T) *gameState {
	t.Helper()
	g := newGameState(nil)
	g.rng = rand.New(rand.NewSource(42))
	return g
}

func TestCruiseKinematics_FasterAndStraighter(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = 3
	// Force-spawn one normal + one cruise shahed with the same base config.
	normal := g.makeShahed(false)
	cruise := g.makeShahed(true)
	if !cruise.Cruise {
		t.Fatal("cruise flag must be set")
	}
	if cruise.Cruise == normal.Cruise {
		t.Fatal("normal/cruise should differ on the flag")
	}
	// Cruise spawn speed should be ~3× normal-band; both vary so we check the
	// floor: cruise must exceed any normal-band possible value (baseSpeed × 1).
	// Normal speed bounds at wave 3: [0.96, 1.26]. Cruise: [2.88, 3.78].
	if cruise.Speed < normal.Speed*2.0 {
		t.Errorf("cruise.Speed=%f should be ~3x normal.Speed=%f", cruise.Speed, normal.Speed)
	}
	// Cruise amplitude smaller.
	if cruise.Amplitude > normal.Amplitude {
		t.Errorf("cruise.Amplitude=%f should be smaller than normal.Amplitude=%f", cruise.Amplitude, normal.Amplitude)
	}
	// Run 5 ticks. Cruise should descend faster (larger Y delta) than normal.
	normalY0 := normal.Y
	cruiseY0 := cruise.Y
	g.shaheds = []*shahed{normal, cruise}
	g.radars = nil // no radar.detection noise
	g.phase = phaseCombat
	for i := 0; i < 5; i++ {
		g.updateShahed(normal)
		g.updateShahed(cruise)
	}
	if cruise.Y-cruiseY0 <= normal.Y-normalY0 {
		t.Errorf("cruise should descend faster: normal Δy=%f cruise Δy=%f",
			normal.Y-normalY0, cruise.Y-cruiseY0)
	}
}

func TestWavePatterns_DispatcherCoversAllSeven(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = 8
	g.radars = nil
	g.phase = phaseCombat
	// Construct a shahed per pattern, run 60 ticks, verify each produced a
	// non-trivial path[] sample. Patterns 0/2/6 use sin; 1 is zigzag delta;
	// 3 is jinking; 4 is straight; 5 is diagonal drift.
	for p := 0; p < 7; p++ {
		s := g.makeShahed(false)
		s.Pattern = p
		s.BaseX = 400
		s.X = 400
		s.PrevX = 400
		s.Y = 50
		for i := 0; i < 60; i++ {
			g.updateShahed(s)
		}
		if s.Path == nil || s.Path.n == 0 {
			t.Errorf("pattern %d: path[] should have samples", p)
		}
		// Pattern 4 (straight) should have minimal lateral drift (just wobble).
		if p == 4 {
			samples := s.Path.snapshot()
			minX := math.Inf(1)
			maxX := math.Inf(-1)
			for _, pt := range samples {
				if pt.X < minX {
					minX = pt.X
				}
				if pt.X > maxX {
					maxX = pt.X
				}
			}
			if maxX-minX > 30 {
				t.Errorf("pattern 4 (straight) drifted laterally %f; expected <30", maxX-minX)
			}
		}
	}
}

func TestPathRing_FixedCapacity(t *testing.T) {
	var r pathRing
	for i := 0; i < pathRingLen+10; i++ {
		r.push(float64(i), float64(i))
	}
	if r.n != pathRingLen {
		t.Fatalf("ring should cap at %d; got n=%d", pathRingLen, r.n)
	}
	snap := r.snapshot()
	if len(snap) != pathRingLen {
		t.Fatalf("snapshot length=%d want %d", len(snap), pathRingLen)
	}
	// Oldest sample should be (pathRingLen+10) - pathRingLen samples ago = i=10.
	if snap[0].X != 10 {
		t.Errorf("snapshot[0].X=%f; expected oldest=10 (after wraparound)", snap[0].X)
	}
	if snap[len(snap)-1].X != float64(pathRingLen+10-1) {
		t.Errorf("snapshot[last].X=%f; expected newest=%d", snap[len(snap)-1].X, pathRingLen+10-1)
	}
	// Reset clears.
	r.reset()
	if r.n != 0 || r.snapshot() != nil {
		t.Fatalf("reset() should clear; got n=%d", r.n)
	}
}

func TestCamelSpitCollision_FlipsShahedToRetreat(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = camelWave
	g.phase = phaseCombat
	g.camelMode = true
	// One shahed mid-air, one camel right beneath it. Set the spit cooldown
	// to zero so the camel fires this tick.
	s := &shahed{
		EID:   1,
		X:     400,
		Y:     500,
		Alive: true,
		Path:  &pathRing{},
	}
	g.shaheds = []*shahed{s}
	c := &camel{
		EID:          2,
		X:            400,
		VX:           0,
		Dir:          1,
		SpitCooldown: 0,
		Alive:        true,
	}
	g.camels = []*camel{c}
	g.updateCamels()
	if len(g.camelSpits) == 0 {
		t.Fatal("camel should have spit this tick")
	}
	// Force the spit straight at the shahed and advance until collision.
	spit := g.camelSpits[0]
	spit.X = s.X
	spit.Y = s.Y - 5
	spit.VX = 0
	spit.VY = 0
	for i := 0; i < 5 && !s.Retreating; i++ {
		g.updateCamels()
	}
	if !s.Retreating {
		t.Fatal("shahed should be flipped to retreating by spit collision")
	}
	if !spit.Alive == false {
		// (Spit should have died; truthy check is awkward but explicit.)
	}
	if spit.Alive {
		t.Error("spit should have died on impact")
	}
}

func TestBunkerOutcome_SafeWhenRunCommittedEarly(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = bunkerRunWave
	g.phase = phaseCombat
	g.bunkerPhase = "active"
	g.bunkerTimer = bunkerWarningTicks
	g.bunkerOutcome = "pending"
	// Commit RUN immediately — the longest possible buffer.
	g.startBunkerRun()
	if g.bunkerRunTimer != 1 {
		t.Fatalf("startBunkerRun should set timer to 1; got %d", g.bunkerRunTimer)
	}
	// Advance through the entire warning period.
	for g.bunkerPhase == "active" {
		g.updateBunker()
	}
	if g.bunkerPhase != "post" {
		t.Fatalf("phase should transition to post; got %s", g.bunkerPhase)
	}
	if g.bunkerOutcome != "safe" {
		t.Errorf("outcome should be safe; got %s", g.bunkerOutcome)
	}
	// Advance through post.
	for g.bunkerPhase == "post" {
		g.updateBunker()
	}
	if g.lives <= 0 {
		t.Errorf("safe outcome must not drop lives; got %d", g.lives)
	}
}

func TestBunkerOutcome_HitWhenRunNotCommitted(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = bunkerRunWave
	g.phase = phaseCombat
	g.lives = 5
	g.bunkerPhase = "active"
	g.bunkerTimer = bunkerWarningTicks
	g.bunkerOutcome = "pending"
	// Do NOT call startBunkerRun. Advance through warning.
	for g.bunkerPhase == "active" {
		g.updateBunker()
	}
	if g.bunkerOutcome != "hit" {
		t.Errorf("outcome should be hit; got %s", g.bunkerOutcome)
	}
	// Advance through post — lives should be zeroed.
	for g.bunkerPhase == "post" {
		g.updateBunker()
	}
	if g.lives != 0 {
		t.Errorf("hit outcome must zero lives; got %d", g.lives)
	}
}

func TestBunkerOutcome_HitWhenRunCommittedTooLate(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = bunkerRunWave
	g.phase = phaseCombat
	g.lives = 5
	g.bunkerPhase = "active"
	g.bunkerTimer = bunkerWarningTicks
	g.bunkerOutcome = "pending"
	// Advance until well past the safe-RUN window. We need bunkerRunTimer to
	// reach < bunkerRunTicks when the warning expires; the only way to commit
	// "too late" is if warning expires within bunkerRunTicks of commit. Move
	// to a state where there are 10 ticks of warning left + then commit.
	for g.bunkerTimer > 10 {
		g.updateBunker()
	}
	g.startBunkerRun()
	for g.bunkerPhase == "active" {
		g.updateBunker()
	}
	if g.bunkerOutcome != "hit" {
		t.Errorf("outcome should be hit when run committed too late; got %s", g.bunkerOutcome)
	}
}

func TestLauncherMove_ClampsDXAndPosition(t *testing.T) {
	g := newDeterministicGame(t)
	g.launchers = []*launcher{{X: 400}}
	// dx within range — applied as-is.
	if !g.moveLauncher(0, 1.0) {
		t.Fatal("valid move should return true")
	}
	if g.launchers[0].X != 401 {
		t.Errorf("X should be 401 after dx=+1; got %f", g.launchers[0].X)
	}
	// dx clamped to ±launcherMoveMaxDX.
	g.launchers[0].X = 400
	g.moveLauncher(0, 10.0) // > launcherMoveMaxDX (6.0) → clamps
	if g.launchers[0].X != 400+launcherMoveMaxDX {
		t.Errorf("dx=10 should clamp to %f; got X=%f", launcherMoveMaxDX, g.launchers[0].X)
	}
	// X clamped to [40, worldW-40].
	g.launchers[0].X = 40
	g.moveLauncher(0, -1.5)
	if g.launchers[0].X != 40 {
		t.Errorf("X should clamp to 40; got %f", g.launchers[0].X)
	}
	g.launchers[0].X = float64(worldW - 40)
	g.moveLauncher(0, 1.5)
	if g.launchers[0].X != float64(worldW-40) {
		t.Errorf("X should clamp to worldW-40=%d; got %f", worldW-40, g.launchers[0].X)
	}
	// Bad launcher idx returns false.
	if g.moveLauncher(99, 1.0) {
		t.Error("bad launcher idx should return false")
	}
}

func TestPathReset_OnPhaseTransitionOutOfCombat(t *testing.T) {
	g := newDeterministicGame(t)
	g.phase = phaseCombat
	g.lives = 1
	// Spawn one shahed, give it some path samples.
	s := g.makeShahed(false)
	g.shaheds = append(g.shaheds, s)
	for i := 0; i < 10; i++ {
		g.updateShahed(s)
	}
	if s.Path.n == 0 {
		t.Fatal("path should have samples after 10 updateShahed calls")
	}
	// Force a wave-end transition: no spawns remaining + no alive shaheds.
	s.Alive = false
	g.shahedsToSpawn = 0
	g.checkWaveEnd()
	if g.phase != phaseWaveEnd {
		t.Fatalf("checkWaveEnd should have flipped to WAVE_END; got %s", g.phase)
	}
	if s.Path.n != 0 {
		t.Errorf("path should be reset on transition out of COMBAT; got n=%d", s.Path.n)
	}
}

func TestCamelMode_ActivatesOnWave6(t *testing.T) {
	g := newDeterministicGame(t)
	g.wave = camelWave - 1
	g.startEmplacement() // wave++ → camelWave
	g.startCombat()
	if !g.camelMode {
		t.Errorf("camel mode should activate on wave %d", camelWave)
	}
	if g.camelPool != camelPoolStart {
		t.Errorf("camel pool should be %d; got %d", camelPoolStart, g.camelPool)
	}
	// releaseCamel decrements pool + appends one.
	g.releaseCamel()
	if g.camelPool != camelPoolStart-1 {
		t.Errorf("releaseCamel should decrement pool; got %d", g.camelPool)
	}
	if len(g.camels) != 1 {
		t.Errorf("releaseCamel should spawn one camel; got %d", len(g.camels))
	}
}

func TestCelebrationConst_LengthMatchesDesign(t *testing.T) {
	// CELEBRATION at 30 Hz = 60 ticks = 2 seconds. Matches index.html L390-392.
	if celebrationTicks != 60 {
		t.Errorf("celebrationTicks=%d; design doc says 60 (2s at 30Hz)", celebrationTicks)
	}
}

func TestSnapshot_EmitsBunkerWhenActive(t *testing.T) {
	g := newDeterministicGame(t)
	g.phase = phaseCombat
	g.bunkerPhase = "active"
	g.bunkerTimer = 100
	g.bunkerOutcome = "pending"
	snap := g.snapshotMap()
	b, ok := snap["bunker"]
	if !ok || b == nil {
		t.Fatal("bunker block should be non-nil when phase != idle")
	}
	bm := b.(map[string]any)
	if bm["phase"] != "active" {
		t.Errorf("bunker.phase=%v want active", bm["phase"])
	}
	if bm["timer"] != 100 {
		t.Errorf("bunker.timer=%v want 100", bm["timer"])
	}
	// Idle → nil.
	g.bunkerPhase = "idle"
	snap = g.snapshotMap()
	if snap["bunker"] != nil {
		t.Errorf("bunker block should be nil when phase=idle; got %v", snap["bunker"])
	}
}

func TestSnapshot_PathOnlyDuringCombat(t *testing.T) {
	g := newDeterministicGame(t)
	g.phase = phaseCombat
	s := g.makeShahed(false)
	s.Path.push(1, 1)
	s.Path.push(2, 2)
	g.shaheds = []*shahed{s}
	snap := g.snapshotMap()
	shads := snap["shaheds"].([]map[string]any)
	if _, ok := shads[0]["path"]; !ok {
		t.Error("path should be present during COMBAT")
	}
	// Flip to WAVE_END — path must not be emitted.
	g.phase = phaseWaveEnd
	snap = g.snapshotMap()
	shads = snap["shaheds"].([]map[string]any)
	if _, ok := shads[0]["path"]; ok {
		t.Error("path should NOT be present outside COMBAT (bandwidth)")
	}
}

func TestWaveSummaryCallback_FiresOnCombatToWaveEnd(t *testing.T) {
	g := newDeterministicGame(t)
	g.phase = phaseCombat
	g.lives = 5
	g.shahedsToSpawn = 0
	var calledWave int
	var calledKills int
	var calledLeaked int
	g.onWaveSummary = func(wave int, kills []killSnapshot, leaked int, _ []pathRecord, _ []pathRecord) {
		calledWave = wave
		calledKills = len(kills)
		calledLeaked = leaked
	}
	// Stash a fake kill + leak so the callback has something to flush.
	g.wave = 3
	g.killsThisWave = []killSnapshot{{ShahedEID: 1, KillX: 100, KillY: 200}}
	g.leakedShaheds = 2
	g.checkWaveEnd()
	if calledWave != 3 {
		t.Errorf("callback wave=%d want 3", calledWave)
	}
	if calledKills != 1 {
		t.Errorf("callback kills=%d want 1", calledKills)
	}
	if calledLeaked != 2 {
		t.Errorf("callback leaked=%d want 2", calledLeaked)
	}
}
