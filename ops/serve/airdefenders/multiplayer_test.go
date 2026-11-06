// Multiplayer tests — focused on the reconnect-token lifecycle, CELEBRATION
// transition gating, and the per-room input-rate limiters.
package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestReconnectToken_RestoresPlayerID(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)

	// First connection — fresh joiner gets monotonic player_id = 1.
	pid1, color1, name1, reconnected1 := h.allocatePlayer("ALICE", "")
	if reconnected1 {
		t.Fatal("first allocation should not be a reconnect")
	}
	if pid1 != 1 {
		t.Fatalf("first player_id should be 1; got %d", pid1)
	}
	if color1 == "" {
		t.Fatal("color should be set")
	}

	// Simulate her connection closing — Hub holds the slot for 30 s.
	p := &Player{ID: pid1, Name: name1, Color: color1, Intercepts: 5, Shots: 12,
		ReconnectToken: "deadbeef"}
	h.retainReconnectToken(p.ReconnectToken, p)

	// Bob joins meanwhile — should get player_id = 2.
	pid2, _, _, _ := h.allocatePlayer("BOB", "")
	if pid2 != 2 {
		t.Fatalf("second fresh player should get player_id 2; got %d", pid2)
	}

	// Alice reconnects with her token — should restore player_id = 1.
	pidR, _, nameR, reconnectedR := h.allocatePlayer("", "deadbeef")
	if !reconnectedR {
		t.Fatal("reconnect with valid token should be flagged as reconnect")
	}
	if pidR != pid1 {
		t.Fatalf("reconnect should restore player_id %d; got %d", pid1, pidR)
	}
	if nameR != "ALICE" {
		t.Fatalf("reconnect should preserve name ALICE; got %q", nameR)
	}

	// Token is single-use — second reconnect with same token must fail (treated as fresh).
	pidR2, _, _, reconnectedR2 := h.allocatePlayer("CHARLIE", "deadbeef")
	if reconnectedR2 {
		t.Fatal("token should be consumed; second use must not reconnect")
	}
	if pidR2 != 3 {
		t.Fatalf("post-consumed token should yield fresh player_id 3; got %d", pidR2)
	}
}

func TestReconnectToken_ExpiresAfter30s(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)

	pid, color, _, _ := h.allocatePlayer("DAVE", "")
	p := &Player{ID: pid, Name: "DAVE", Color: color, ReconnectToken: "expireme"}

	// Stash the slot with an already-elapsed expiry.
	h.mu.Lock()
	h.reconnectTokens[p.ReconnectToken] = &reconnectSlot{
		playerID:  p.ID,
		name:      p.Name,
		color:     p.Color,
		expiresAt: time.Now().Add(-1 * time.Second),
	}
	h.mu.Unlock()

	// Reconnect with expired token — should NOT restore.
	pidR, _, _, reconnected := h.allocatePlayer("DAVE", "expireme")
	if reconnected {
		t.Fatal("expired token must not restore identity")
	}
	if pidR == pid {
		t.Fatalf("expired token should yield fresh ID; got same %d", pidR)
	}

	// Sweep — slot should be evicted.
	h.expireTokens()
	h.mu.Lock()
	_, present := h.reconnectTokens["expireme"]
	h.mu.Unlock()
	if present {
		t.Fatal("expired slot should be swept")
	}
}

func TestReconnectToken_UnknownTokenTreatedAsFresh(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	pid, _, _, reconnected := h.allocatePlayer("EVE", "no-such-token")
	if reconnected {
		t.Fatal("unknown token should not be flagged as reconnect")
	}
	if pid != 1 {
		t.Fatalf("unknown token should mint fresh ID 1; got %d", pid)
	}
}

// testOpenDB — minimal in-test SQLite. The shared openDB() lives in main.go
// alongside the production path defaults; we replicate the schema migrate
// step here so tests don't depend on the file-system lookup of /data.
func testOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scores.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		os.RemoveAll(dir)
	})
	return db
}

func TestShouldCelebrate_TopFiveTriggers(t *testing.T) {
	db := testOpenDB(t)
	bus := newEventBus()
	h := newHub(bus, db, nil, nil)
	r := h.room

	// Seed: 4 scores higher than 1000, 1 lower. The player's 1000 is rank 5.
	// shouldCelebrate filters mode='mp' (so SP top-5s don't trigger MP
	// celebration), so seed rows must be MP for the test to be a valid
	// regression check of the celebration gate.
	now := time.Now().UnixMilli()
	for i, s := range []int64{5000, 4000, 3000, 2000, 500} {
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO scores (name, score, wave, created_at, mode) VALUES (?, ?, ?, ?, 'mp')`,
			"P"+itoa(i), s, 1, now-int64(i*1000))
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if !r.shouldCelebrate(1000) {
		t.Errorf("score=1000 (rank 5) should trigger CELEBRATION")
	}
	if r.shouldCelebrate(100) {
		t.Errorf("score=100 (rank 6) should NOT trigger CELEBRATION")
	}
	// Rank 1 — beating everything.
	if !r.shouldCelebrate(10000) {
		t.Errorf("score=10000 (rank 1) should trigger CELEBRATION")
	}
}

func TestShouldCelebrate_HandlesEmptyLeaderboard(t *testing.T) {
	db := testOpenDB(t)
	bus := newEventBus()
	h := newHub(bus, db, nil, nil)
	r := h.room
	// First-ever score lands at rank 1.
	if !r.shouldCelebrate(1) {
		t.Errorf("first score should trigger CELEBRATION (rank 1)")
	}
}

func TestPlayerPalette_Deterministic(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	// Player 1 → palette[1], 2 → palette[2], ..., 9 → palette[1] (mod 8).
	for i := 1; i <= 10; i++ {
		_, color, _, _ := h.allocatePlayer("X", "")
		want := playerPalette[uint32(i)%uint32(len(playerPalette))]
		if color != want {
			t.Errorf("player %d color: want %s got %s", i, want, color)
		}
	}
}

// ─── Per-player launcher ownership ─────────────────────────────────────
// Tests below cover the join → spawn / disconnect → preserve / sweep →
// despawn lifecycle. They drive the Room state directly (no goroutine /
// tick loop) so they're deterministic; the Room.run loop's join branch is
// emulated inline by calling r.g.spawnLauncherForOwner / setting LauncherSlot,
// mirroring what `case p := <-r.joinCh` does in the production loop.

// emulateJoin — mirrors the Room.run joinCh branch for test purposes.
// Adds the player to r.players, spawns or reattaches the launcher.
func emulateJoin(t *testing.T, r *Room, p *Player) {
	t.Helper()
	r.players[p.ID] = p
	if existing := r.g.launcherIdxForOwner(p.ID); existing >= 0 {
		p.LauncherSlot = r.g.launchers[existing].Slot
		return
	}
	_, slot, ok := r.g.spawnLauncherForOwner(p.ID)
	if !ok {
		t.Fatalf("emulateJoin: no free slot for player %d", p.ID)
	}
	p.LauncherSlot = slot
}

func TestLauncherSpawnOnJoin(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	pA := &Player{ID: 1, Name: "A", LauncherSlot: -1}
	pB := &Player{ID: 2, Name: "B", LauncherSlot: -1}
	pC := &Player{ID: 3, Name: "C", LauncherSlot: -1}
	emulateJoin(t, r, pA)
	emulateJoin(t, r, pB)
	emulateJoin(t, r, pC)

	if len(r.g.launchers) != 3 {
		t.Fatalf("3 joins should yield 3 launchers; got %d", len(r.g.launchers))
	}
	// Distinct slots + distinct owners.
	slots := map[int]bool{}
	owners := map[uint32]bool{}
	for _, L := range r.g.launchers {
		if slots[L.Slot] {
			t.Errorf("slot %d reused while another launcher holds it", L.Slot)
		}
		slots[L.Slot] = true
		if owners[L.OwnerPlayerID] {
			t.Errorf("owner %d holds two launchers", L.OwnerPlayerID)
		}
		owners[L.OwnerPlayerID] = true
	}
	// Slots are first-free; with three fresh joins we expect 0, 1, 2.
	for i, want := range []int{0, 1, 2} {
		if r.g.launchers[i].Slot != want {
			t.Errorf("launcher[%d].Slot=%d want %d", i, r.g.launchers[i].Slot, want)
		}
	}
	// Slot X positions land in [120, worldW-120], evenly spaced.
	for _, L := range r.g.launchers {
		if L.X < 120 || L.X > float64(worldW-120) {
			t.Errorf("launcher X=%f outside [120, %d]", L.X, worldW-120)
		}
	}
	// OwnerPlayerID matches Player.ID for each player.
	for _, p := range []*Player{pA, pB, pC} {
		idx := r.g.launcherIdxForOwner(p.ID)
		if idx < 0 {
			t.Errorf("player %d should own a launcher", p.ID)
			continue
		}
		if r.g.launchers[idx].OwnerPlayerID != p.ID {
			t.Errorf("launcher[%d].OwnerPlayerID=%d want %d", idx, r.g.launchers[idx].OwnerPlayerID, p.ID)
		}
	}
}

func TestLauncherDespawnOnTimeout(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	p := &Player{ID: 1, Name: "Z", LauncherSlot: -1, ReconnectToken: "tok-timeout"}
	emulateJoin(t, r, p)
	if r.g.launcherIdxForOwner(p.ID) < 0 {
		t.Fatal("launcher should exist post-join")
	}
	// Mark disconnected + stash reconnect slot with already-elapsed expiry.
	p.disconnected = true
	h.mu.Lock()
	h.reconnectTokens[p.ReconnectToken] = &reconnectSlot{
		playerID:     p.ID,
		expiresAt:    time.Now().Add(-1 * time.Second),
		launcherSlot: p.LauncherSlot,
	}
	h.mu.Unlock()
	// Sweep — slot expired, launcher should be despawned + player removed.
	r.sweepDisconnected()
	if r.g.launcherIdxForOwner(p.ID) >= 0 {
		t.Error("launcher should be despawned after reconnect window expires")
	}
	if _, present := r.players[p.ID]; present {
		t.Error("player should be removed from roster after sweep")
	}
}

func TestLauncherDespawnPreservedOnReconnect(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	p := &Player{ID: 1, Name: "Q", LauncherSlot: -1, ReconnectToken: "tok-reconnect"}
	emulateJoin(t, r, p)
	priorIdx := r.g.launcherIdxForOwner(p.ID)
	if priorIdx < 0 {
		t.Fatal("launcher should exist post-join")
	}
	priorX := r.g.launchers[priorIdx].X
	priorSlot := r.g.launchers[priorIdx].Slot

	// Simulate WS close — mark disconnected + retain token.
	p.disconnected = true
	h.retainReconnectToken(p.ReconnectToken, p)
	// Within 30s, sweep should NOT despawn (slot still alive).
	r.sweepDisconnected()
	if r.g.launcherIdxForOwner(p.ID) < 0 {
		t.Fatal("launcher should remain during 30s reconnect window")
	}

	// Now reconnect: allocate returns the original player_id + slot.
	_, _, _, isReconn, _, _, gotSlot := h.allocatePlayerWithCounters("", "tok-reconnect")
	if !isReconn {
		t.Fatal("token should restore as reconnect")
	}
	if gotSlot != priorSlot {
		t.Errorf("reconnect should restore launcher slot %d; got %d", priorSlot, gotSlot)
	}
	// Emulate the rejoin (launcher already exists — reattaches by owner).
	p.disconnected = false
	p.LauncherSlot = gotSlot
	emulateJoin(t, r, p)
	idx := r.g.launcherIdxForOwner(p.ID)
	if idx < 0 {
		t.Fatal("launcher should still exist post-reconnect")
	}
	if r.g.launchers[idx].X != priorX {
		t.Errorf("reconnected launcher X moved: was %f, now %f", priorX, r.g.launchers[idx].X)
	}
	if r.g.launchers[idx].Slot != priorSlot {
		t.Errorf("reconnected launcher slot changed: was %d, now %d", priorSlot, r.g.launchers[idx].Slot)
	}
}

func TestInputFireNotOwnerDropped(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room
	r.g.phase = phaseCombat

	pA := &Player{ID: 1, Name: "A", LauncherSlot: -1}
	pB := &Player{ID: 2, Name: "B", LauncherSlot: -1}
	emulateJoin(t, r, pA)
	emulateJoin(t, r, pB)
	bLauncherIdx := r.g.launcherIdxForOwner(pB.ID)
	if bLauncherIdx < 0 {
		t.Fatal("setup: B should own a launcher")
	}
	// Reset metric counter for not_owner.
	dropsBefore := droppedInputsSnapshot()["not_owner"]
	// A sends input.fire targeting B's launcher idx (which is advisory anyway —
	// server resolves to A's owned launcher, A.spaceCooldown should still set
	// and fire A's launcher, not B's). To prove "not_owner" semantics we test
	// the case where A has no launcher.
	r.g.despawnLauncherForOwner(pA.ID) // simulate A's launcher gone
	pA.LauncherSlot = -1
	// Re-resolve B's launcher idx — despawn shifts the slice, so the
	// pre-despawn index may now point at the wrong launcher (or past
	// the end of the slice).
	bLauncherIdx = r.g.launcherIdxForOwner(pB.ID)
	if bLauncherIdx < 0 {
		t.Fatal("setup: B's launcher should still exist after A's despawn")
	}
	ammoBefore := r.g.launchers[bLauncherIdx].InterceptorsLeft
	r.applyInput(InputEvent{PlayerID: pA.ID, Type: "input.fire", LauncherIdx: bLauncherIdx})
	if r.g.launchers[bLauncherIdx].InterceptorsLeft != ammoBefore {
		t.Error("A's fire should not consume B's ammo")
	}
	if pA.spaceCooldown != 0 {
		t.Error("A's spaceCooldown should not be set when no launcher owned")
	}
	dropsAfter := droppedInputsSnapshot()["not_owner"]
	if dropsAfter <= dropsBefore {
		t.Errorf("not_owner drop counter should increment: before=%d after=%d", dropsBefore, dropsAfter)
	}
}

func TestInputFireModeOwnerOnly(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	pA := &Player{ID: 1, Name: "A", LauncherSlot: -1}
	pB := &Player{ID: 2, Name: "B", LauncherSlot: -1}
	emulateJoin(t, r, pA)
	emulateJoin(t, r, pB)
	aIdx := r.g.launcherIdxForOwner(pA.ID)
	bIdx := r.g.launcherIdxForOwner(pB.ID)

	// A changes A's own fire mode — should land.
	r.applyInput(InputEvent{PlayerID: pA.ID, Type: "input.fire_mode", LauncherIdx: aIdx, Mode: 1})
	if r.g.launchers[aIdx].FireMode != 1 {
		t.Errorf("A's fire_mode should be 1; got %d", r.g.launchers[aIdx].FireMode)
	}
	if r.g.launchers[bIdx].FireMode != 0 {
		t.Errorf("B's fire_mode should be untouched (0); got %d", r.g.launchers[bIdx].FireMode)
	}
	dropsBefore := droppedInputsSnapshot()["not_owner"]
	// A attempts to change B's fire mode — should drop.
	r.applyInput(InputEvent{PlayerID: pA.ID, Type: "input.fire_mode", LauncherIdx: bIdx, Mode: 2})
	if r.g.launchers[bIdx].FireMode != 0 {
		t.Errorf("A should not be able to change B's fire_mode; B.FireMode=%d", r.g.launchers[bIdx].FireMode)
	}
	dropsAfter := droppedInputsSnapshot()["not_owner"]
	if dropsAfter <= dropsBefore {
		t.Errorf("not_owner drop counter should increment: before=%d after=%d", dropsBefore, dropsAfter)
	}
	// Same shape for input.resupply.
	r.g.phase = phaseCombat
	resupplyBefore := r.g.launchers[bIdx].Resupplying
	r.applyInput(InputEvent{PlayerID: pA.ID, Type: "input.resupply", LauncherIdx: bIdx})
	if r.g.launchers[bIdx].Resupplying != resupplyBefore {
		t.Errorf("A should not be able to resupply B's launcher")
	}
	// Same shape for input.launcher_move.
	bX := r.g.launchers[bIdx].X
	r.applyInput(InputEvent{PlayerID: pA.ID, Type: "input.launcher_move", LauncherIdx: bIdx, DX: 1.0})
	if r.g.launchers[bIdx].X != bX {
		t.Errorf("A should not be able to move B's launcher; X moved %f → %f", bX, r.g.launchers[bIdx].X)
	}
}

func TestSlotReusedAfterDespawn(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	pA := &Player{ID: 1, Name: "A", LauncherSlot: -1}
	pB := &Player{ID: 2, Name: "B", LauncherSlot: -1}
	pC := &Player{ID: 3, Name: "C", LauncherSlot: -1}
	emulateJoin(t, r, pA)
	emulateJoin(t, r, pB)
	emulateJoin(t, r, pC)
	bSlot := r.players[pB.ID].LauncherSlot
	bX := r.g.launchers[r.g.launcherIdxForOwner(pB.ID)].X
	// B vanishes (sweep evicted past the reconnect window).
	r.g.despawnLauncherForOwner(pB.ID)
	delete(r.players, pB.ID)
	// New joiner D — should take B's freed slot (first-free wins).
	pD := &Player{ID: 4, Name: "D", LauncherSlot: -1}
	emulateJoin(t, r, pD)
	dIdx := r.g.launcherIdxForOwner(pD.ID)
	if dIdx < 0 {
		t.Fatal("D should own a launcher")
	}
	if r.g.launchers[dIdx].Slot != bSlot {
		t.Errorf("D should reuse freed slot %d; got %d", bSlot, r.g.launchers[dIdx].Slot)
	}
	if r.g.launchers[dIdx].X != bX {
		t.Errorf("D's launcher X should match the freed slot's X (%f); got %f", bX, r.g.launchers[dIdx].X)
	}
}

func TestLauncherSlotXPositions_Deterministic(t *testing.T) {
	// Snapshot the slot table — anchors the values the design doc lists.
	// With worldW=800 and 8 slots: 120, 200, 280, 360, 440, 520, 600, 680.
	want := []float64{120, 200, 280, 360, 440, 520, 600, 680}
	for i, w := range want {
		got := launcherSlotX(i)
		if got != w {
			t.Errorf("launcherSlotX(%d)=%f; want %f", i, got, w)
		}
	}
}

// TestLaunchersPersistAcrossGameOverReset — regression for the live-test bug
// where launchers vanished from state.tick.launchers[] after the first game
// ended. Root cause: gameState.reset() (called on GAME_OVER → TITLE via
// advanceState) was nuking g.launchers along with the per-game ephemerals.
//
// Contract (schema doc §"Rooms and session lifecycle" + design doc
// §"Shared state model"): launchers belong to Players, not Games. They
// persist across game resets as long as their owner stays connected; only
// sweepDisconnected (after the 30 s reconnect window) may despawn them.
// FireMode + X also persist; in-flight Arming/Resupplying are cleared;
// InterceptorsLeft refills.
func TestLaunchersPersistAcrossGameOverReset(t *testing.T) {
	bus := newEventBus()
	h := newHub(bus, nil, nil, nil)
	r := h.room

	pA := &Player{ID: 1, Name: "A", LauncherSlot: -1}
	pB := &Player{ID: 2, Name: "B", LauncherSlot: -1}
	emulateJoin(t, r, pA)
	emulateJoin(t, r, pB)

	aIdx := r.g.launcherIdxForOwner(pA.ID)
	bIdx := r.g.launcherIdxForOwner(pB.ID)
	if aIdx < 0 || bIdx < 0 {
		t.Fatal("both players should own a launcher post-join")
	}
	// Capture pre-reset identity + persisted-across-reset state.
	aPre := *r.g.launchers[aIdx]
	bPre := *r.g.launchers[bIdx]
	// Deliberately set FireMode + nudge X away from the slot default so we
	// can prove they survive the reset.
	r.g.launchers[aIdx].FireMode = 2
	r.g.launchers[bIdx].FireMode = 1
	r.g.launchers[aIdx].X = aPre.X + 25
	r.g.launchers[bIdx].X = bPre.X - 30
	// Set some in-flight ephemerals that SHOULD be cleared by reset.
	r.g.launchers[aIdx].Arming = true
	r.g.launchers[aIdx].ArmTimer = 12
	r.g.launchers[aIdx].ArmingByPlayerID = pA.ID
	r.g.launchers[bIdx].Resupplying = true
	r.g.launchers[bIdx].ResupplyTimer = 30
	// Drain one launcher's ammo so we can confirm the refill.
	r.g.launchers[aIdx].InterceptorsLeft = 0

	// Drive the game to GAME_OVER and trigger the TITLE-reset transition the
	// same way input.state_advance does in production.
	r.g.phase = phaseGameOver
	r.g.lives = 0
	r.g.score = 1234

	if !r.g.advanceState() {
		t.Fatal("advanceState on GAME_OVER should transition (calls reset → startEmplacement)")
	}

	// Launcher slice survives the reset — both players still have their launchers.
	if got := len(r.g.launchers); got != 2 {
		t.Fatalf("post-reset launcher count: want 2; got %d", got)
	}
	aIdxAfter := r.g.launcherIdxForOwner(pA.ID)
	bIdxAfter := r.g.launcherIdxForOwner(pB.ID)
	if aIdxAfter < 0 {
		t.Fatal("player A's launcher should persist across GAME_OVER reset")
	}
	if bIdxAfter < 0 {
		t.Fatal("player B's launcher should persist across GAME_OVER reset")
	}
	aPost := r.g.launchers[aIdxAfter]
	bPost := r.g.launchers[bIdxAfter]

	// Identity preserved: same owner, same slot.
	if aPost.OwnerPlayerID != pA.ID {
		t.Errorf("A's launcher OwnerPlayerID=%d; want %d", aPost.OwnerPlayerID, pA.ID)
	}
	if bPost.OwnerPlayerID != pB.ID {
		t.Errorf("B's launcher OwnerPlayerID=%d; want %d", bPost.OwnerPlayerID, pB.ID)
	}
	if aPost.Slot != aPre.Slot {
		t.Errorf("A's slot changed across reset: was %d, now %d", aPre.Slot, aPost.Slot)
	}
	if bPost.Slot != bPre.Slot {
		t.Errorf("B's slot changed across reset: was %d, now %d", bPre.Slot, bPost.Slot)
	}

	// X position preserved (player aimed it there deliberately).
	if aPost.X != aPre.X+25 {
		t.Errorf("A's X should persist across reset: want %f, got %f", aPre.X+25, aPost.X)
	}
	if bPost.X != bPre.X-30 {
		t.Errorf("B's X should persist across reset: want %f, got %f", bPre.X-30, bPost.X)
	}

	// FireMode preserved (player's deliberate selection).
	if aPost.FireMode != 2 {
		t.Errorf("A's FireMode should persist across reset: want 2, got %d", aPost.FireMode)
	}
	if bPost.FireMode != 1 {
		t.Errorf("B's FireMode should persist across reset: want 1, got %d", bPost.FireMode)
	}

	// In-flight ephemerals cleared.
	if aPost.Arming || aPost.ArmTimer != 0 || aPost.ArmingByPlayerID != 0 {
		t.Errorf("A's arming state should be cleared across reset: %+v", *aPost)
	}
	if bPost.Resupplying || bPost.ResupplyTimer != 0 {
		t.Errorf("B's resupply state should be cleared across reset: %+v", *bPost)
	}

	// Ammo refilled (advanceState ran startEmplacement, but the new wave's
	// per-launcher allocation runs on startCombat — between the two, reset's
	// own refill must have left non-zero ammo on both launchers).
	if aPost.InterceptorsLeft == 0 {
		t.Error("A's launcher ammo should be refilled post-reset (was 0 pre-reset)")
	}
	if bPost.InterceptorsLeft == 0 {
		t.Error("B's launcher ammo should be refilled post-reset")
	}

	// Per-game scalars wiped.
	if r.g.score != 0 || r.g.lives != maxLives || r.g.intercepts != 0 {
		t.Errorf("per-game scalars not reset: score=%d lives=%d intercepts=%d",
			r.g.score, r.g.lives, r.g.intercepts)
	}
}

// ─── MP leaderboard auto-submit ────────────────────────────────────────
// Block of tests covers:
//   - members CSV is the contribution-ordered list of normalised names
//   - autoSubmitScore inserts an MP row with team + members + mode='mp'
//   - queryTop's mode filter returns the right subset for sp / mp / unfiltered

func TestMembersCSV_OrderAndNormalisation(t *testing.T) {
	ranks := []rankEntry{
		{Name: "alice", Rank: 1},
		{Name: " bob ", Rank: 2},
		{Name: "carol@@@", Rank: 3},
		{Name: "", Rank: 4}, // empty → skipped
	}
	got := membersCSV(ranks)
	want := "ALICE,BOB,CAROL"
	if got != want {
		t.Errorf("membersCSV: got %q want %q", got, want)
	}
}

func TestAutoSubmitScore_PersistsMpRow(t *testing.T) {
	db := testOpenDB(t)
	bus := newEventBus()
	h := newHub(bus, db, newRateLimiter(5), newSSEHub())
	r := h.room

	r.autoSubmitScore("ALICE+2", 1234, 5, "ALICE+2", "ALICE,BOB,CAROL")

	scores, err := queryTop(context.Background(), db, 0, 10, "")
	if err != nil {
		t.Fatalf("queryTop: %v", err)
	}
	if len(scores) != 1 {
		t.Fatalf("want 1 row; got %d", len(scores))
	}
	s := scores[0]
	if s.Name != "ALICE+2" || s.Score != 1234 || s.Wave != 5 {
		t.Errorf("row fields: %+v", s)
	}
	if s.Team != "ALICE+2" {
		t.Errorf("team not persisted: got %q want %q", s.Team, "ALICE+2")
	}
	if s.Members != "ALICE,BOB,CAROL" {
		t.Errorf("members not persisted: got %q", s.Members)
	}
	if s.Mode != "mp" {
		t.Errorf("mode should be 'mp'; got %q", s.Mode)
	}
}

func TestQueryTop_FiltersByMode(t *testing.T) {
	db := testOpenDB(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	// Seed: two SP rows, two MP rows.
	for i, row := range []struct {
		name, mode string
		score      int64
	}{
		{"ALICE", "sp", 100},
		{"BOB", "sp", 200},
		{"CAROL", "mp", 300},
		{"DAN", "mp", 400},
	} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO scores (name, score, wave, created_at, team, members, mode) VALUES (?, ?, 1, ?, '', '', ?)`,
			row.name, row.score, now+int64(i), row.mode); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// mode="" → all 4 rows.
	all, err := queryTop(ctx, db, 0, 10, "")
	if err != nil {
		t.Fatalf("queryTop all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("unfiltered: want 4 rows, got %d", len(all))
	}
	// mode="sp" → 2 rows, neither named CAROL/DAN.
	sp, err := queryTop(ctx, db, 0, 10, "sp")
	if err != nil {
		t.Fatalf("queryTop sp: %v", err)
	}
	if len(sp) != 2 {
		t.Errorf("sp: want 2 rows, got %d", len(sp))
	}
	for _, s := range sp {
		if s.Mode != "sp" {
			t.Errorf("sp filter leaked mode=%q row name=%q", s.Mode, s.Name)
		}
	}
	// mode="mp" → 2 rows, both with mode='mp'.
	mp, err := queryTop(ctx, db, 0, 10, "mp")
	if err != nil {
		t.Fatalf("queryTop mp: %v", err)
	}
	if len(mp) != 2 {
		t.Errorf("mp: want 2 rows, got %d", len(mp))
	}
	for _, s := range mp {
		if s.Mode != "mp" {
			t.Errorf("mp filter leaked mode=%q row name=%q", s.Mode, s.Name)
		}
	}
}

func TestAdvanceFromGameOverLandsOnTitle(t *testing.T) {
	// Regression: previously advanceState on GAME_OVER went straight to
	// EMPLACEMENT (auto-restart). New behaviour returns to TITLE so the
	// player sees the home screen + leaderboard + name edit affordance
	// before opting into a fresh round.
	db := testOpenDB(t)
	bus := newEventBus()
	h := newHub(bus, db, newRateLimiter(5), newSSEHub())
	r := h.room

	p := &Player{ID: 1, Name: "ALICE"}
	emulateJoin(t, r, p)
	r.g.phase = phaseGameOver
	r.g.lives = 0
	r.applyInput(InputEvent{PlayerID: p.ID, Type: "input.state_advance"})
	if r.g.phase != phaseTitle {
		t.Errorf("post-advance phase: want %q, got %q", phaseTitle, r.g.phase)
	}
}
