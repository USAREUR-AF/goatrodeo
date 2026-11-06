# Air Defenders — Shared Leaderboard

## Conclusion

Air Defenders is a self-contained Go service that serves the game HTML (embedded) and a shared high-score API. Storage is SQLite. Live updates ride Server-Sent Events. The browser keeps a localStorage mirror so the board still renders in island mode, and queues unsent submissions for replay when the network returns.

Before this change, "high scores" meant `localStorage` — private to each browser. Users reasonably thought the feature was missing.

## Topology

```text
browser ──HTTPS──▶ Traefik (airdefenders.<site>.<zone>; authenticated door)
                         │ mesh-only middleware (100.64.0.0/10)
                         ▼
                 airdefenders (Go, :8095)
                   ├── GET  /                    → index.html (embedded)
                   ├── GET  /api/scores          → top 10, period-filtered
                   ├── POST /api/scores          → submit {name, score, wave}
                   ├── GET  /api/scores/stream   → SSE (live insertions)
                   ├── GET  /health              → db ping
                   └── GET  /metrics             → Prometheus
                         │
                         ▼
                 /data/scores.db (SQLite WAL, bind-mount)
```

One domain, one container, one process. No separate database container, no cross-container compose, no external auth.

## Stack

- **Go 1.22** (stdlib `net/http` with pattern-matched routes)
- **SQLite** via `modernc.org/sqlite` (pure-Go, no CGO → fully static binary)
- **Base images by role** `go-build` (build) + `runtime-static` (runtime), `ops/base-images/` (goatcore ADR 1220): public defaults `golang` + distroless static; an environment's overlay may name Chainguard's
- **Server-Sent Events** for live leaderboard pushes (reconnect handled by `EventSource` in the browser)
- **Prometheus** text exposition, hand-rolled (no client library; six counters + three gauges)

## Data model

```sql
CREATE TABLE scores (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    score      INTEGER NOT NULL,
    wave       INTEGER NOT NULL,
    created_at INTEGER NOT NULL,             -- unix millis
    team       TEXT    NOT NULL DEFAULT '',  -- MP auto-derived label ("ALICE+2"); empty for SP
    members    TEXT    NOT NULL DEFAULT '',  -- MP comma-joined contributor names (rank order); empty for SP
    mode       TEXT    NOT NULL DEFAULT 'sp' -- "sp" or "mp"; splits the leaderboard
);
CREATE INDEX idx_scores_score      ON scores(score DESC);
CREATE INDEX idx_scores_created_at ON scores(created_at DESC);
CREATE INDEX idx_scores_mode       ON scores(mode);
```

Inserts append. After each insert, the table is pruned to the top 100 rows by `(score DESC, created_at ASC)` — rows below the top 100 are deleted in the same transaction as the insert. No other deletes; the only UPDATE path in the codebase was the abandoned team-name flow (now removed). Period filtering is a range scan on `created_at` within the top-100 pool.

The `mode` column splits SP and MP leaderboards. `GET /api/scores?mode=sp` returns only single-player rows; `?mode=mp` returns only multiplayer rows; omitting the param returns both (back-compat). Pre-migration rows default to `mode='sp'`, which is correct for the legacy POST-only path. `team` + `members` are populated only on MP auto-submit and are blank for SP rows. The team string is computed server-side from the contributor ranks at game-over time (e.g. solo → top player's name; N≥2 → `"<top>+<N-1>"`) — no client input.

Migration on existing DBs is additive: `migrateScores` probes `PRAGMA table_info(scores)` and runs `ALTER TABLE ... ADD COLUMN` for any missing column. SQLite doesn't support `ADD COLUMN IF NOT EXISTS` so the probe-first pattern is necessary.

## Name constraints

Names are uppercase, 1-14 chars, `[A-Z0-9 _-]`. Server-side `normaliseName` enforces. Browser `<input maxlength="14">` mirrors the server cap; older browser data with 12-char names keeps working.

## Period filtering

`GET /api/scores?period={daily|weekly|monthly|yearly|all}` maps to `created_at >= cutoff` where cutoff is `now-24h`, `now-7d`, `now-30d`, `Jan 1 of current year`, or `0`. Returns top 10 ordered by `(score DESC, created_at ASC)` so earlier submissions break score ties.

Combine with `&mode=sp` or `&mode=mp` to filter by game mode. Both clients use this: `index.html` fetches `?period=…&mode=sp`, `mp.html` fetches `?period=all&mode=mp`. The TITLE screen of each client renders its top 5 from that filtered response so SP runs and 8-player team scores never share a column.

## Game-over polish

- **TOP FIVE celebration.** If the submitted score ranks in the top 5 of the current leaderboard (computed client-side against the cached server list), the game enters a 2-second `CELEBRATION` state before the normal game-over screen: full-canvas confetti (120 particles, gravity + rotation, recycled at the top) plus a flashing `TOP FIVE!` banner with the player's rank + score. After 2 s the state transitions to `GAME_OVER`. No new endpoint — pure client-side detection against `serverScores.all`.
- **Rotating game-over phrase.** On confirmName, a phrase is picked from a fixed 4-entry list sequentially (index persisted in `localStorage.airdefenders_phrase_idx`) and rendered above `AREA COMPROMISED` on the game-over screen. Phrases are verbatim — no grammar correction, by design.

## Offline behaviour (island mode)

The browser maintains two localStorage keys:

- `airdefenders_scores` — mirror of recent entries (for display when offline)
- `airdefenders_pending` — unsent submissions (retry queue)

Flow:

1. On submit, always write to the mirror. Then `POST /api/scores`.
2. On POST failure, enqueue to `airdefenders_pending`.
3. Every 30 s and on successful list-fetch, drain the pending queue.
4. When the server is unreachable, `filterScores()` falls back to the mirror and the board shows `OFFLINE — LOCAL CACHE`.

Scores submitted while offline on two different browsers will both be replayed when each comes back online — no deduplication across devices because the client has no stable identity. Acceptable tradeoff for a game.

## Forgeability

The game runs entirely in the browser. Anyone can `curl -X POST /api/scores -d '{"name":"pwn","score":999999,"wave":999}'`. This is accepted because:

- The service is mesh-internal (Traefik `ipallowlist.sourcerange=100.64.0.0/10`). Only enrolled peers can reach it.
- The board has no social or operational weight.
- A naive per-IP rate limit (5 submissions/min) stops accidental floods.

If the board ever needed to be trustworthy, the path is either (a) sign each submission with an HMAC over a server-issued session token plus the hash of game events, or (b) move scoring server-side by streaming game events over WebSocket. Neither is built.

## Observability

`GET /metrics` exposes:

| Metric | Type | Meaning |
| --- | --- | --- |
| `airdefenders_submissions_total` | counter | All POST attempts |
| `airdefenders_submissions_ok` | counter | Successful inserts |
| `airdefenders_rejected_rate_limit` | counter | 429s issued |
| `airdefenders_rejected_validation` | counter | 400s issued |
| `airdefenders_queries_total` | counter | Leaderboard reads |
| `airdefenders_scores_total` | gauge | Rows in table |
| `airdefenders_top_score` | gauge | Current max score |
| `airdefenders_sse_clients` | gauge | Active live-stream subscribers |

Smoketest ([ops/smoketest/services.yaml](../../ops/smoketest/services.yaml)) probes `/`, `/health`, and `/api/scores?period=all`. Status page ([ops/status/check.sh](../../ops/status/check.sh)) auto-discovers `/health` from the `health_path` field in `services.yaml`.

## File layout

```text
ops/serve/airdefenders/
├── main.go              — HTTP handlers, DB, SSE hub, rate limiter, metrics
├── schema.sql           — reference copy of embedded schema
├── index.html           — game (moved from ops/serve/airdefenders.html)
├── go.mod / go.sum
├── Dockerfile           — multi-stage on the base-image roles (static binary)
└── docker-compose.yml   — dev-local compose (Ansible writes its own on host)

ops/ansible/
├── airdefenders.yml                              — deploy playbook (mirrors efforts.yml)
└── templates/airdefenders-docker-compose.yml.j2  — host compose
```

## Deployment

```bash
ansible-playbook ops/ansible/airdefenders.yml
```

Included in `site.yml` and `site-services.yml` after `efforts.yml`.

## Multiplayer (co-op) — landed 2026-05-20

Server-authoritative co-op multiplayer was added in the same package as a precursor to (but distinct from) the Step 9 fabric migration below. One new endpoint `GET /api/mp/ws` ([nhooyr.io/websocket](https://pkg.go.dev/nhooyr.io/websocket)) runs a 30 Hz tick; clients render 30 Hz snapshots interpolated to 60 fps; lives/score/wave are shared; solo play is a 1-player room. The four engagement-relevant moments (detection, shot fired, intercept, impact) emit on an in-process `EventBus` consumed by both the existing leaderboard hub (game-over auto-submit) and an opt-in Zenoh publisher (a runtime `ZENOH_PUBLISH=1` switch — the build tag was dropped in the REST pivot, so one static image serves every goatnet). The publisher targets `airdefenders/<site>/<event>/v1` under the `airdefenders` vendor on the goatnet's trial fabric (the fabric demo and its admin-only trigger are in [`docs/operations/airdefenders-zenoh-demo.md`](../operations/airdefenders-zenoh-demo.md)). Full contract — WS schema, event-bus interface, envelope shape, failure modes — at [`airdefenders-multiplayer.md`](airdefenders-multiplayer.md).

## Future — planned in Block 11 Step 9

The current SQLite backend is a **precursor**. Planned migration is to the Zenoh data fabric, not Postgres+Spock. The migration is not airdefenders-specific work — it delivers three reusable platform patterns that every future fabric-publishing app inherits. **Status (2026-05-20):** the multiplayer slice above wired the four engagement events through to Zenoh — that exercises the publish surface but does NOT satisfy Step 9. Step 9's platform-pattern work (schema registry + validator, NTP-attested envelope, dual-write SQLite→fabric leaderboard migration, stateful Pk consumer, nightly population profile) remains unchanged below.

- **Fabric-backed storage + replication.** Scores become an event stream on `airdefenders/{site}/session/{id}/wave/summary`. Zenoh's RocksDB storage handles persistence (30-day TTL on raw, 1-year on rollups), router federation handles multi-site propagation for free. Full schema: [`airdefenders-telemetry.md`](airdefenders-telemetry.md).
- **Richer telemetry** beyond scores: in-game detections, shots, outcomes and resupply. A synthetic traffic source with a realistic schema shape.
- **Per-player tactical profile + population comparison** via a nightly batch analytics job.
- **Migration tooling.** Pre-fabric SQLite scores replay into the fabric via a one-shot migrator so no data is lost. During transition the Go server dual-writes to SQLite + fabric; read path switches at the end of Step 9C.

Platform design docs landing alongside the migration (applicable to every future fabric app):

- [`event-telemetry-schema.md`](event-telemetry-schema.md) — keyexpr hierarchy, envelope, NTP attestation, Go + JS client modules
- [`fabric-stateful-consumers.md`](fabric-stateful-consumers.md) — correlation + session timeout + DLQ + history replay for the Pk computation
- [`fabric-schema-evolution.md`](fabric-schema-evolution.md) — schema registry + validator + CI enforcement + migration tools

Implementation sequence + gates: [`zenoh-implementation.md §Step 9`](../project/zenoh-implementation.md#step-9--second-app-dogfood-air-defenders-telemetry--platform-patterns-post-gate).

## Interim-only ideas (superseded by fabric migration)

- **Anti-cheat.** Add HMAC-signed submissions keyed to a short-lived session token. Only worth doing if the board becomes operationally visible BEFORE Step 9 lands. Post-Step-9, signing integrates with the envelope and applies uniformly across every fabric publisher.
- **Longer history.** `GET /api/scores?period=all&limit=100` — trivial SQLite extension if needed pre-Step-9. Post-Step-9 this is a Zenoh range query.
