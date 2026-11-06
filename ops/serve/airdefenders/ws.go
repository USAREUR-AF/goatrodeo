// WebSocket — handshake + reader + writer goroutines + ping/pong.
//
// Endpoint: GET /api/mp/ws. Schema doc §"WebSocket endpoint" is canon.
//
// Per-connection structure:
//
//	        ┌───────────────────┐
//	 READ ──┤ reader goroutine  ├── inputs ──▶ Room.inputCh
//	        └───────────────────┘
//	        ┌───────────────────┐
//	WRITE ◀─┤ writer goroutine  │◀── frames ◀── Player.outbound
//	        └───────────────────┘
//	        ┌───────────────────┐
//	 PING ──┤  ping goroutine   ├── periodic ping; reader resets idle deadline
//	        └───────────────────┘
//
// Concurrency: only one reader and one writer goroutine per conn.
// nhooyr/websocket allows one concurrent reader + one writer, and we respect
// that here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

const (
	// schema doc §"WebSocket endpoint". Inbound reads are small (input verbs
	// are <200 B); outbound state.tick frames can hit ~6-8 KiB during a busy
	// COMBAT tick (path[] history × ~10 entities + camels + camel_spits +
	// bunker block). nhooyr.io/websocket applies the read limit only to
	// incoming messages, so 16 KiB is comfortable headroom against client
	// frames we don't yet anticipate.
	maxMessageBytes = 16 * 1024
	readIdleTimeout = 25 * time.Second
	writeTimeout    = 5 * time.Second
	pingInterval    = 10 * time.Second
	pongDeadline    = 15 * time.Second // not directly enforced; we rely on read deadline
	// defaultMaxConnsPerIP — a single household/NAT should be able to field a
	// full co-op room (maxPlayers=8), and the auto-pilot bot fleet spawns up to
	// 8 browser windows from one laptop IP. Override with AD_MAX_CONNS_PER_IP
	// (e.g. raise it for a shared relay, lower it to harden a public deployment).
	defaultMaxConnsPerIP = 8
	outboundBuffer       = 64 // frames buffered per writer
	inputQueueDepth      = 64 // per-player input queue depth
)

// maxConnsPerIP resolves the per-IP WebSocket connection cap from the
// AD_MAX_CONNS_PER_IP env var, falling back to defaultMaxConnsPerIP. Resolved
// per-handshake (cheap) so an operator can retune without a redeploy churn.
func maxConnsPerIP() int {
	if v := os.Getenv("AD_MAX_CONNS_PER_IP"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxConnsPerIP
}

// closeCode → schema doc §"Close codes".
const (
	closeNormal       = 1000
	closeGoingAway    = 1001
	closeServerError  = 1011
	closeInvalidHello = 4000
	closeTokenUnknown = 4001
	closeRateLimited  = 4002
	closeServerFull   = 4003
)

// handleMPWS — http handler installed at GET /api/mp/ws.
func handleMPWS(hub *Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !hub.reserveIP(ip, maxConnsPerIP()) {
			http.Error(w, "too many connections from this IP", http.StatusTooManyRequests)
			return
		}

		// Origin policy: we delegate the actual check to acceptOrigin() below
		// (mesh-internal posture per docs/design/airdefenders-multiplayer.md
		// §WebSocket endpoint). nhooyr's InsecureSkipVerify accepts the
		// handshake without origin enforcement; we then run our own check.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
			CompressionMode:    websocket.CompressionDisabled,
		})
		if err != nil {
			hub.releaseIP(ip)
			log.Printf("ws accept: %v", err)
			return
		}
		// Re-evaluate origin per the schema doc: "accept any origin only when
		// Host matches airdefenders.* or Origin is empty". The mesh ACL is the
		// real auth surface; this just enforces the doc's stated posture.
		if !acceptOrigin(r) {
			c.Close(closeInvalidHello, "origin not allowed")
			hub.releaseIP(ip)
			return
		}

		c.SetReadLimit(maxMessageBytes)
		hub.wsConnections.Add(1)
		defer hub.wsConnections.Add(-1)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Outbound + input chans for this connection. Owned by this handler.
		outbound := make(chan []byte, outboundBuffer)
		inputs := make(chan InputEvent, inputQueueDepth)

		var player *Player
		defer func() {
			hub.releaseIP(ip)
			if player != nil {
				// Room.Leave queues onto the tick goroutine which marks the
				// player disconnected AND stashes the reconnect-token slot
				// from inside the tick (no race vs. concurrent counter
				// updates).
				hub.room.Leave(player.ID)
			}
			c.Close(closeNormal, "")
		}()

		// Step 1 — first frame must be `hello`.
		helloCtx, helloCancel := context.WithTimeout(ctx, 5*time.Second)
		_, raw, err := c.Read(helloCtx)
		helloCancel()
		if err != nil {
			c.Close(closeInvalidHello, "hello timeout")
			return
		}
		var hello struct {
			Type           string `json:"type"`
			Name           string `json:"name"`
			ReconnectToken string `json:"reconnect_token"`
		}
		if err := json.Unmarshal(raw, &hello); err != nil || hello.Type != "hello" {
			c.Close(closeInvalidHello, "invalid hello")
			return
		}
		name, _ := normaliseName(hello.Name)
		// Reject if room is full (only fresh joiners count; reconnects bypass cap).
		// We hold the Hub mu briefly to atomically check + allocate.
		hub.mu.Lock()
		isFresh := hello.ReconnectToken == "" || hub.reconnectTokens[hello.ReconnectToken] == nil
		fullRoom := isFresh && hub.players.Load() >= maxPlayers
		hub.mu.Unlock()
		if fullRoom {
			c.Close(closeServerFull, "room full")
			return
		}

		pid, color, finalName, _, priorIntercepts, priorShots, priorLauncherSlot := hub.allocatePlayerWithCounters(name, hello.ReconnectToken)
		// Mint a fresh reconnect token for THIS connection. (If they reconnect
		// later, a new one is minted each time.)
		token := newReconnectToken()
		player = &Player{
			ID:             pid,
			Name:           finalName,
			Color:          color,
			JoinedAt:       time.Now().UnixMilli(),
			Intercepts:     priorIntercepts,
			Shots:          priorShots,
			ReconnectToken: token,
			inputs:         inputs,
			outbound:       outbound,
			// Reconnect: priorLauncherSlot carries the slot the player had
			// before disconnect. Fresh joiner: -1; Room.run's joinCh branch
			// allocates a slot on first observation. The game-state launcher
			// for a reconnecting player is still alive in r.g.launchers (the
			// leaveCh branch does NOT despawn — only sweepDisconnected does
			// on slot expiry), so the room reattaches by owner-id rather
			// than re-spawning. See Room.run.
			LauncherSlot: priorLauncherSlot,
		}
		hub.room.Join(player)

		// Step 2 — synchronous snapshot + roster for the welcome payload.
		snap := hub.room.Snapshot(ctx)
		roster := hub.room.RosterJSON(ctx)
		var snapMap map[string]any
		if snap != nil {
			_ = json.Unmarshal(snap, &snapMap)
		}
		// Strip the top-level type/tick/ts so the embedded snapshot reads as the
		// state.tick body the renderer expects (with `type:state.tick` still
		// present so client code paths can dispatch on it uniformly).
		// We leave it as-is; the renderer's first state.tick will arrive in milliseconds anyway.
		var rosterEntries []any
		if roster != nil {
			var rosterEnvelope struct {
				Roster []any `json:"roster"`
			}
			_ = json.Unmarshal(roster, &rosterEnvelope)
			rosterEntries = rosterEnvelope.Roster
		}
		if rosterEntries == nil {
			rosterEntries = []any{}
		}

		welcome := map[string]any{
			"type":            "welcome",
			"player_id":       pid,
			"reconnect_token": token,
			"room_id":         "global",
			"server_tick_hz":  tickHz,
			"world":           map[string]int{"w": worldW, "h": worldH},
			"color":           color,
			"snapshot":        snapMap,
			"roster":          rosterEntries,
		}
		welcomeBytes, _ := json.Marshal(welcome)
		// Send welcome synchronously before starting writer goroutine, so the
		// client sees it as the first frame.
		writeCtx, writeCancel := context.WithTimeout(ctx, writeTimeout)
		err = c.Write(writeCtx, websocket.MessageText, welcomeBytes)
		writeCancel()
		if err != nil {
			log.Printf("ws welcome write: %v", err)
			return
		}

		// Step 3 — start writer + ping + reader.
		writerDone := make(chan struct{})
		go writerLoop(ctx, c, outbound, writerDone)
		go pingLoop(ctx, c)

		readerLoop(ctx, c, hub, player)

		// Reader exited — cancel + wait for writer to drain its current frame.
		cancel()
		select {
		case <-writerDone:
		case <-time.After(writeTimeout + time.Second):
		}
	}
}

// acceptOrigin — schema doc policy.
func acceptOrigin(r *http.Request) bool {
	host := strings.ToLower(r.Host)
	origin := strings.ToLower(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if strings.HasPrefix(host, "airdefenders.") {
		return true
	}
	// Same-host check (origin == "http(s)://<host>").
	if strings.Contains(origin, host) {
		return true
	}
	return false
}

// readerLoop — drains the WS conn, decodes JSON frames, queues inputs onto
// the room's input channel. Exits on any read error or context cancel.
func readerLoop(ctx context.Context, c *websocket.Conn, hub *Hub, player *Player) {
	for {
		// Reset deadline each frame: idle timeout = readIdleTimeout.
		readCtx, readCancel := context.WithTimeout(ctx, readIdleTimeout)
		_, data, err := c.Read(readCtx)
		readCancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				// Normal client-side close shows up as closed-status error; don't spam.
				closeErr := websocket.CloseStatus(err)
				if closeErr == -1 {
					log.Printf("ws read: %v", err)
				}
			}
			return
		}
		ev, ok := decodeInput(player.ID, data)
		if !ok {
			droppedInputsInc("malformed")
			continue
		}
		ev.ReceivedAt = time.Now()
		hub.room.SubmitInput(ev)
	}
}

// writerLoop — drains the outbound chan and writes to the WS. Exits on close.
func writerLoop(ctx context.Context, c *websocket.Conn, outbound <-chan []byte, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-outbound:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(writeCtx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// pingLoop — periodic application-level ping (the schema doc names it
// `ping` rather than relying on websocket-protocol pings, so clients can
// echo `pong` with a nonce).
func pingLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			frame, _ := json.Marshal(map[string]any{
				"type":  "ping",
				"nonce": newEventID()[:12],
			})
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(writeCtx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// decodeInput — parses one JSON frame into an InputEvent.
func decodeInput(playerID uint32, data []byte) (InputEvent, bool) {
	var raw struct {
		Type           string  `json:"type"`
		LauncherIdx    int     `json:"launcher_idx"`
		Mode           int     `json:"mode"`
		Aim            *Vec2   `json:"aim"`
		InterceptorEID uint32  `json:"interceptor_eid"`
		RadarIdx       int     `json:"radar_idx"`
		Pos            *Vec2   `json:"pos"`
		Echo           string  `json:"echo"`
		Name           string  `json:"name"`
		DX             float64 `json:"dx"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return InputEvent{}, false
	}
	switch raw.Type {
	case "input.fire",
		"input.launcher_switch",
		"input.launcher_move",
		"input.fire_mode",
		"input.resupply",
		"input.steer",
		"input.bunker_run",
		"input.camel_release",
		"input.radar_place",
		"input.state_advance",
		"input.set_name",
		"hello", // post-handshake hello = "update my display name" per Agent 2's integration note
		"pong":
		// recognised
	default:
		return InputEvent{}, false
	}
	// Treat "hello" as a name-update intent. Normalise here so the tick
	// goroutine never has to.
	if raw.Type == "hello" {
		raw.Type = "input.set_name"
	}
	return InputEvent{
		PlayerID:       playerID,
		Type:           raw.Type,
		LauncherIdx:    raw.LauncherIdx,
		Mode:           raw.Mode,
		Aim:            raw.Aim,
		InterceptorEID: raw.InterceptorEID,
		RadarIdx:       raw.RadarIdx,
		Pos:            raw.Pos,
		PongEcho:       raw.Echo,
		Name:           raw.Name,
		DX:             raw.DX,
	}, true
}
