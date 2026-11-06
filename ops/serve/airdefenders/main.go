// Air Defenders — self-contained game server.
//
// Serves the game HTML (embedded) and a shared high-score leaderboard.
// Storage: SQLite (pure-Go, modernc.org/sqlite — no CGO).
// Live updates: Server-Sent Events at /api/scores/stream.
// Metrics: Prometheus at /metrics.
//
// Endpoints:
//
//	GET  /                     — game HTML (embedded)
//	GET  /mp                   — co-op multiplayer client (?bot=<persona> auto-plays)
//	GET  /demo                 — one-page fabric demo: 8 bot personae in an iframe grid
//	                             (platform admins only — demo_auth.go)
//	GET  /api/me               — who the viewer is + whether they may run the demo
//	GET  /api/scores?period=X  — top 10 by period (daily|weekly|monthly|yearly|all)
//	POST /api/scores           — submit {name, score, wave}
//	GET  /api/scores/stream    — SSE: live score inserts
//	GET  /metrics              — Prometheus
//	GET  /health               — liveness + DB check
//
// Forgeability: scores are client-reported and unsigned. Acceptable for a
// mesh-internal game. See docs/design/airdefenders-leaderboard.md §forgeability.
package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed index.html
var indexHTML []byte

//go:embed mp.html
var mpHTML []byte

//go:embed demo.html
var demoHTML []byte

// ─── Config ────────────────────────────────────────────────

type config struct {
	Port     string
	DBPath   string
	MaxPerIP int
}

func loadConfig() config {
	return config{
		Port:     envOr("PORT", "8095"),
		DBPath:   envOr("DB_PATH", "/data/scores.db"),
		MaxPerIP: 5, // submissions per minute per IP
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ─── Types ─────────────────────────────────────────────────

type Score struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Score     int64  `json:"score"`
	Wave      int    `json:"wave"`
	CreatedAt int64  `json:"date"` // unix millis (matches legacy localStorage shape)
	// Team + Members are populated for multiplayer rows. Empty for single-player
	// and for pre-migration rows. Members is a comma-joined list of normalised
	// player names in contribution-rank order (highest first). Team is the
	// auto-derived display label (e.g. "ALICE+2") computed server-side at
	// game-over time — no user input.
	Team    string `json:"team,omitempty"`
	Members string `json:"members,omitempty"`
	// Mode — "sp" for single-player submissions (the POST /api/scores path)
	// or "mp" for the multiplayer auto-submit. Lets the leaderboard show two
	// independent top-N lists since 8-player team scores aren't directly
	// comparable to solo runs.
	Mode string `json:"mode,omitempty"`
}

type Submission struct {
	Name  string `json:"name"`
	Score int64  `json:"score"`
	Wave  int    `json:"wave"`
	Mode  string `json:"mode"` // optional — defaults to "sp"
}

// ─── DB ────────────────────────────────────────────────────

const schema = `
CREATE TABLE IF NOT EXISTS scores (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    score      INTEGER NOT NULL,
    wave       INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    team       TEXT NOT NULL DEFAULT '',
    members    TEXT NOT NULL DEFAULT '',
    mode       TEXT NOT NULL DEFAULT 'sp'
);
CREATE INDEX IF NOT EXISTS idx_scores_score      ON scores(score DESC);
CREATE INDEX IF NOT EXISTS idx_scores_created_at ON scores(created_at DESC);
-- NOTE: idx_scores_mode is created in migrateScores(), NOT here. This bootstrap
-- DDL runs before migrateScores adds the mode column, so on a pre-existing
-- pre-mode database the CREATE TABLE IF NOT EXISTS above is a no-op and an index
-- on scores(mode) here would fail with "no such column: mode" before the column
-- exists — crash-looping the server. Creating it post-migration keeps both fresh
-- and upgraded databases happy. Regression: TestOpenDBMigratesLegacySchema.
`

// migrateScores — additive ALTER TABLE migration that lifts pre-team databases
// (rows created before the team/members/mode columns existed) up to the
// current schema. SQLite has no `ADD COLUMN IF NOT EXISTS`, so we probe via
// PRAGMA table_info first.
//
// Lifecycle: all existing rows on a migrated DB default to mode='sp'. That's
// correct for legacy SP-only deployments and slightly mislabels any MP rows
// inserted between the prior team-name commit and this revert (the trial DB
// is fresh so this is not in practice an issue).
func migrateScores(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(scores)`)
	if err != nil {
		return fmt.Errorf("table_info: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan table_info: %w", err)
		}
		have[name] = true
	}
	rows.Close()
	if !have["team"] {
		if _, err := db.Exec(`ALTER TABLE scores ADD COLUMN team TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add team col: %w", err)
		}
	}
	if !have["members"] {
		if _, err := db.Exec(`ALTER TABLE scores ADD COLUMN members TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add members col: %w", err)
		}
	}
	if !have["mode"] {
		if _, err := db.Exec(`ALTER TABLE scores ADD COLUMN mode TEXT NOT NULL DEFAULT 'sp'`); err != nil {
			return fmt.Errorf("add mode col: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_scores_mode ON scores(mode)`); err != nil {
		return fmt.Errorf("create mode index: %w", err)
	}
	return nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := migrateScores(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func periodCutoff(period string, now time.Time) int64 {
	switch period {
	case "daily":
		return now.Add(-24 * time.Hour).UnixMilli()
	case "weekly":
		return now.Add(-7 * 24 * time.Hour).UnixMilli()
	case "monthly":
		return now.Add(-30 * 24 * time.Hour).UnixMilli()
	case "yearly":
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()).UnixMilli()
	default: // "all" or anything else
		return 0
	}
}

// queryTop — returns top-N rows with optional period + mode filtering.
//   mode == "" → no mode filter (back-compat for any consumer that hasn't
//                opted into the SP/MP split).
//   mode == "sp" or "mp" → WHERE mode = ?.
//
// Period is enforced via cutoff (callers compute via periodCutoff).
func queryTop(ctx context.Context, db *sql.DB, cutoff int64, limit int, mode string) ([]Score, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if mode == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT id, name, score, wave, created_at, team, members, mode
			FROM scores
			WHERE created_at >= ?
			ORDER BY score DESC, created_at ASC
			LIMIT ?`, cutoff, limit)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT id, name, score, wave, created_at, team, members, mode
			FROM scores
			WHERE created_at >= ? AND mode = ?
			ORDER BY score DESC, created_at ASC
			LIMIT ?`, cutoff, mode, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Score, 0, limit)
	for rows.Next() {
		var s Score
		if err := rows.Scan(&s.ID, &s.Name, &s.Score, &s.Wave, &s.CreatedAt, &s.Team, &s.Members, &s.Mode); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ─── Validation ────────────────────────────────────────────

var nameClean = regexp.MustCompile(`[^A-Z0-9 _-]`)

func normaliseName(raw string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(raw))
	n = nameClean.ReplaceAllString(n, "")
	if n == "" {
		n = "ANON"
	}
	if len(n) > 14 {
		n = n[:14]
	}
	return n, nil
}

func validateSubmission(s Submission) error {
	if s.Score < 0 || s.Score > 10_000_000 {
		return errors.New("score out of range")
	}
	if s.Wave < 0 || s.Wave > 10_000 {
		return errors.New("wave out of range")
	}
	return nil
}

// ─── Rate limiter ──────────────────────────────────────────
// Simple per-IP token bucket. Enough friction to stop casual spam; not a
// security control.

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	max     int
}

type bucket struct {
	count  int
	window time.Time
}

func newRateLimiter(max int) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, max: max}
}

func (r *rateLimiter) allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[ip]
	if !ok || now.Sub(b.window) > time.Minute {
		r.buckets[ip] = &bucket{count: 1, window: now}
		return true
	}
	if b.count >= r.max {
		return false
	}
	b.count++
	return true
}

// ─── SSE hub ───────────────────────────────────────────────

type sseHub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func newSSEHub() *sseHub { return &sseHub{clients: map[chan []byte]struct{}{}} }

func (h *sseHub) add() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) remove(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *sseHub) broadcast(payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.clients {
		select {
		case ch <- payload:
		default: // drop for slow clients
		}
	}
}

func (h *sseHub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ─── Metrics ───────────────────────────────────────────────
// Hand-rolled Prometheus text exposition — avoids pulling a heavy client.

type metrics struct {
	submissionsTotal atomic.Int64
	submissionsOK    atomic.Int64
	queriesTotal     atomic.Int64
	rejectedRate     atomic.Int64
	rejectedValid    atomic.Int64
}

func (m *metrics) render(hub *sseHub, db *sql.DB, mp *Hub, bus *eventBus) string {
	var topScore int64
	var totalRows int64
	_ = db.QueryRow(`SELECT COALESCE(MAX(score),0), COUNT(*) FROM scores`).Scan(&topScore, &totalRows)

	var sb strings.Builder
	fmt.Fprintf(&sb, "# HELP airdefenders_submissions_total Total POST /api/scores attempts.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_submissions_total counter\n")
	fmt.Fprintf(&sb, "airdefenders_submissions_total %d\n", m.submissionsTotal.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_submissions_ok Successful inserts.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_submissions_ok counter\n")
	fmt.Fprintf(&sb, "airdefenders_submissions_ok %d\n", m.submissionsOK.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_queries_total Leaderboard reads.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_queries_total counter\n")
	fmt.Fprintf(&sb, "airdefenders_queries_total %d\n", m.queriesTotal.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_rejected_rate_limit Rate-limit rejections.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_rejected_rate_limit counter\n")
	fmt.Fprintf(&sb, "airdefenders_rejected_rate_limit %d\n", m.rejectedRate.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_rejected_validation Validation rejections.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_rejected_validation counter\n")
	fmt.Fprintf(&sb, "airdefenders_rejected_validation %d\n", m.rejectedValid.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_scores_total Rows in scores table.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_scores_total gauge\n")
	fmt.Fprintf(&sb, "airdefenders_scores_total %d\n", totalRows)
	fmt.Fprintf(&sb, "# HELP airdefenders_top_score Highest score recorded.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_top_score gauge\n")
	fmt.Fprintf(&sb, "airdefenders_top_score %d\n", topScore)
	fmt.Fprintf(&sb, "# HELP airdefenders_sse_clients Active live-stream subscribers.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_sse_clients gauge\n")
	fmt.Fprintf(&sb, "airdefenders_sse_clients %d\n", hub.count())

	// ─── Multiplayer metrics (schema doc §Metrics) ───
	if mp != nil {
		fmt.Fprintf(&sb, "# HELP airdefenders_mp_ws_connections Open WS connections.\n")
		fmt.Fprintf(&sb, "# TYPE airdefenders_mp_ws_connections gauge\n")
		fmt.Fprintf(&sb, "airdefenders_mp_ws_connections %d\n", mp.ConnCount())
		fmt.Fprintf(&sb, "# HELP airdefenders_mp_rooms Active rooms.\n")
		fmt.Fprintf(&sb, "# TYPE airdefenders_mp_rooms gauge\n")
		fmt.Fprintf(&sb, "airdefenders_mp_rooms %d\n", 1)
		fmt.Fprintf(&sb, "# HELP airdefenders_mp_players Connected players.\n")
		fmt.Fprintf(&sb, "# TYPE airdefenders_mp_players gauge\n")
		fmt.Fprintf(&sb, "airdefenders_mp_players %d\n", mp.PlayerCount())
	}
	fmt.Fprintf(&sb, "# HELP airdefenders_mp_dropped_inputs_total Inputs rejected.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_mp_dropped_inputs_total counter\n")
	for reason, n := range droppedInputsSnapshot() {
		fmt.Fprintf(&sb, "airdefenders_mp_dropped_inputs_total{reason=%q} %d\n", reason, n)
	}
	if bus != nil {
		fmt.Fprintf(&sb, "# HELP airdefenders_mp_eventbus_dropped_total EventBus drops to slow subscribers.\n")
		fmt.Fprintf(&sb, "# TYPE airdefenders_mp_eventbus_dropped_total counter\n")
		for sub, n := range bus.DroppedSnapshot() {
			fmt.Fprintf(&sb, "airdefenders_mp_eventbus_dropped_total{subscriber=%q} %d\n", sub, n)
		}
		fmt.Fprintf(&sb, "# HELP airdefenders_mp_eventbus_subscribers Active EventBus subscribers.\n")
		fmt.Fprintf(&sb, "# TYPE airdefenders_mp_eventbus_subscribers gauge\n")
		fmt.Fprintf(&sb, "airdefenders_mp_eventbus_subscribers %d\n", bus.SubscriberCount())
	}
	fmt.Fprintf(&sb, "# HELP airdefenders_mp_zenoh_publish_errors_total Zenoh publish failures.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_mp_zenoh_publish_errors_total counter\n")
	fmt.Fprintf(&sb, "airdefenders_mp_zenoh_publish_errors_total %d\n", mpZenohPubErrs.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_mp_zenoh_publish_total Zenoh publishes attempted, by event_type.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_mp_zenoh_publish_total counter\n")
	for et, n := range mpZenohPubByTypeSnapshot() {
		fmt.Fprintf(&sb, "airdefenders_mp_zenoh_publish_total{event_type=%q} %d\n", et, n)
	}
	// Tick duration — exposed as a simple cumulative-microseconds counter + tick count;
	// dashboards derive avg. A full histogram would require a heavier client.
	fmt.Fprintf(&sb, "# HELP airdefenders_mp_tick_duration_microseconds_total Cumulative server tick wall time.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_mp_tick_duration_microseconds_total counter\n")
	fmt.Fprintf(&sb, "airdefenders_mp_tick_duration_microseconds_total %d\n", tickDurMicro.Load())
	fmt.Fprintf(&sb, "# HELP airdefenders_mp_ticks_total Total server ticks processed.\n")
	fmt.Fprintf(&sb, "# TYPE airdefenders_mp_ticks_total counter\n")
	fmt.Fprintf(&sb, "airdefenders_mp_ticks_total %d\n", tickCount.Load())
	return sb.String()
}

// ─── Handlers ──────────────────────────────────────────────

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.Index(xf, ","); i > 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(indexHTML)
}

func handleMPPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(mpHTML)
}

// handleDemoPage serves the one-page fabric demo: an 8-cell grid of /mp?bot=<persona>
// iframes that fills the co-op room (maxPlayers=8) from a single browser tab.
// SITE_ID is substituted into the embedded HTML so the on-page watch command
// (goat sub 'airdefenders/<site>/**') and the surface deep-links are correct
// for the goatnet this instance serves.
func handleDemoPage(siteID string) http.HandlerFunc {
	page := bytes.ReplaceAll(demoHTML, []byte("__SITE_ID__"), []byte(siteID))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(page)
	}
}

func handleList(db *sql.DB, m *metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.queriesTotal.Add(1)
		period := r.URL.Query().Get("period")
		if period == "" {
			period = "all"
		}
		mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
		// Whitelist — only known modes get passed through to the query;
		// anything else is treated as "no filter" so a typo doesn't silently
		// return zero rows.
		if mode != "sp" && mode != "mp" {
			mode = ""
		}
		cutoff := periodCutoff(period, time.Now())
		scores, err := queryTop(r.Context(), db, cutoff, 10, mode)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		resp := map[string]interface{}{
			"period": period,
			"scores": scores,
		}
		if mode != "" {
			resp["mode"] = mode
		}
		json.NewEncoder(w).Encode(resp)
	}
}

func handleSubmit(db *sql.DB, rl *rateLimiter, hub *sseHub, m *metrics) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.submissionsTotal.Add(1)
		if !rl.allow(clientIP(r)) {
			m.rejectedRate.Add(1)
			http.Error(w, "rate limited", 429)
			return
		}
		var s Submission
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			m.rejectedValid.Add(1)
			http.Error(w, "invalid json", 400)
			return
		}
		if err := validateSubmission(s); err != nil {
			m.rejectedValid.Add(1)
			http.Error(w, err.Error(), 400)
			return
		}
		name, _ := normaliseName(s.Name)
		mode := strings.ToLower(strings.TrimSpace(s.Mode))
		if mode != "sp" && mode != "mp" {
			mode = "sp" // POST /api/scores is single-player's submission path
		}
		now := time.Now().UnixMilli()
		res, err := db.ExecContext(r.Context(),
			`INSERT INTO scores (name, score, wave, created_at, mode) VALUES (?, ?, ?, ?, ?)`,
			name, s.Score, s.Wave, now, mode)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		id, _ := res.LastInsertId()
		m.submissionsOK.Add(1)

		// Cap the table to the top 100 all-time. Safe under concurrent inserts
		// because the NOT IN subquery runs under SQLite's write lock.
		if _, err := db.ExecContext(r.Context(),
			`DELETE FROM scores WHERE id NOT IN (
				SELECT id FROM scores ORDER BY score DESC, created_at ASC LIMIT 100
			)`); err != nil {
			log.Printf("prune: %v", err)
		}

		entry := Score{ID: id, Name: name, Score: s.Score, Wave: s.Wave, CreatedAt: now, Mode: mode}
		payload, _ := json.Marshal(entry)
		hub.broadcast(payload)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(entry)
	}
}

func handleStream(hub *sseHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		ch := hub.add()
		defer hub.remove(ch)

		fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case payload, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "event: score\ndata: %s\n\n", payload)
				flusher.Flush()
			case <-ping.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

func handleHealth(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok := db.PingContext(r.Context()) == nil
		status := "ok"
		code := 200
		if !ok {
			status = "db_unreachable"
			code = 503
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "db": ok})
	}
}

func handleMetrics(m *metrics, hub *sseHub, db *sql.DB, mp *Hub, bus *eventBus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Write([]byte(m.render(hub, db, mp, bus)))
	}
}

// ─── Main ──────────────────────────────────────────────────

func main() {
	cfg := loadConfig()
	log.Printf("airdefenders starting (port=%s db=%s)", cfg.Port, cfg.DBPath)

	db, err := openDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer db.Close()

	hub := newSSEHub()
	rl := newRateLimiter(cfg.MaxPerIP)
	m := &metrics{}

	// ─── Multiplayer wiring ──────────────────────────────
	// EventBus + multiplayer Hub. EventBus is the in-process pub/sub the
	// game loop calls; Agent 3's optional Zenoh publisher subscribes here.
	bus := newEventBus()
	mp := newHub(bus, db, rl, hub)

	// Optional Zenoh publisher (Agent 3). Always linked; gating is the
	// runtime env var ZENOH_PUBLISH=1. When unset, no publisher subscribes
	// to the bus and no HTTP client is created — the nethost-hosted instance
	// behaves identically to pre-Zenoh.
	//
	// Implementation: pure-stdlib HTTP PUT to the Zenoh router's REST plugin
	// (see zenoh.go for the pivot rationale away from `eclipse-zenoh/zenoh-go`).
	var zenohStop func()
	if os.Getenv("ZENOH_PUBLISH") == "1" {
		zcfg := ZenohConfig{
			RESTEndpoint: os.Getenv("ZENOH_REST_ENDPOINT"),
			SiteID:       envOr("SITE_ID", "nethost"),
			Instance:     os.Getenv("HOSTNAME"),
		}
		stop, zerr := StartZenohPublisher(bus, zcfg)
		if zerr != nil {
			log.Printf("zenoh: %v (continuing without publisher)", zerr)
		} else {
			zenohStop = stop
		}
	}

	// Start tick goroutine + token sweeper. tied to a top-level context so we
	// can clean shutdown.
	mpCtx, mpCancel := context.WithCancel(context.Background())
	mp.Start(mpCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("GET /mp", handleMPPage)
	// The demo fills the whole room, so it is admin-only. Who counts as an
	// admin, and whether the identity headers may be trusted at all, is
	// decided once here from the environment (demo_auth.go).
	gate := newDemoGateFromEnv()
	log.Printf("demo gate: trust_forwarded_identity=%v ungated=%v admin_groups=%s",
		gate.trustForwarded, gate.ungated, envOr("DEMO_ADMIN_GROUPS", defaultDemoAdminGroups))
	mux.HandleFunc("GET /demo", requireDemoAdmin(gate, handleDemoPage(envOr("SITE_ID", "nethost"))))
	mux.HandleFunc("GET /api/me", handleMe(gate))
	mux.HandleFunc("GET /api/scores", handleList(db, m))
	mux.HandleFunc("POST /api/scores", handleSubmit(db, rl, hub, m))
	mux.HandleFunc("GET /api/scores/stream", handleStream(hub))
	mux.HandleFunc("GET /health", handleHealth(db))
	mux.HandleFunc("GET /metrics", handleMetrics(m, hub, db, mp, bus))
	mux.HandleFunc("GET /api/mp/ws", handleMPWS(mp))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout — SSE streams must stay open.
		IdleTimeout: 120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-sigCh
	log.Println("shutting down...")
	mpCancel() // stop the room tick + token sweeper
	if zenohStop != nil {
		zenohStop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
