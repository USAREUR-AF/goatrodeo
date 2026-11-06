# Air Defenders Telemetry — First Caller of the Fabric Application Patterns

## Conclusion

Air Defenders is the first end-to-end caller of three platform patterns landing alongside it:

1. [Event telemetry schema](event-telemetry-schema.md) — keyexpr hierarchy, envelope, NTP attestation, client modules
2. [Fabric stateful consumers](fabric-stateful-consumers.md) — correlation + session + dead-letter pattern (fire → terminate for Pk)
3. [Fabric schema evolution](fabric-schema-evolution.md) — schema registry + validator + CI enforcement + migration tooling

The game produces structured event telemetry, which makes it a synthetic traffic source with a realistic schema shape — useful for fabric load testing and demos.

The [current leaderboard](airdefenders-leaderboard.md) is a SQLite-backed precursor. The migration to fabric-backed telemetry happens in Block 11 Step 9 ([zenoh-implementation.md](../project/zenoh-implementation.md)).

## Keyexpr inventory

All topics rooted under `airdefenders/{site}/session/{session_id}/`. Session ID is a UUID minted per browser session on game start.

| Keyexpr suffix | Rate | Payload highlights |
|---|---|---|
| `/start` | 1/session | player_name, client_version, emplacement_layout_hash |
| `/end` | 1/session | reason (game_over, disconnect, abandoned), final_score, final_wave |
| `/wave/start` | 1/wave | wave_number, threat_count_expected, time_budget_s |
| `/wave/summary` | 1/wave | wave_number, threats, kills, leakers, rounds_used, pk_overall, by_mode_breakdown |
| `/emplacement/layout` | 1/wave | positions of radars + launchers (for replay + heatmaps) |
| `/radar/sweep` | 60/sec (high) | angle, range — **disabled by default** (high-rate, replay-only; gated by `--enable-sweep-stream`) |
| `/radar/detect` | ~10/sec | track_id, x, y, vx, vy, snr, classification_guess, radar_id |
| `/track/update` | ~20/sec (high) | **disabled by default** (fused track state per frame; gated by `--enable-track-stream`) |
| `/threat/assess` | ~5/sec | track_id, priority, tti_s, engagement_recommendation |
| `/engagement/fire` | 1-10/sec | launcher_id, mode (PROX / DIRECT / AUTOPILOT), target_track_id, interceptor_id |
| `/engagement/terminate` | matches fire | interceptor_id, target_track_id, outcome (kill / miss / self_destruct), miss_distance, detonation_xy |
| `/impact` | rare | track_id, impact_xy, damage_level (leaker — reached protected area) |
| `/resupply` | ~1/min | launcher_id, cycle_time_s, rounds_reloaded |

Rate category shapes storage + exporter config. High-rate topics are gated behind feature flags so the default traffic shape matches a realistic profile (events at human timescales) without being dominated by radar-sweep noise.

## Envelope fields specific to airdefenders

Standard envelope from [event-telemetry-schema.md](event-telemetry-schema.md). Additional required fields for airdefenders sources:

```
source_id = session UUID
site_id   = the site where the Go ingest server runs (NOT the player's browser location)
```

Two caveats:

- **The browser is not the source_id origin.** The Go server is. The browser sends events to the Go server; the server stamps `source_id = session_id` and `site_id = server_site_id` before publishing to the fabric. This preserves the invariant that site_id identifies a fabric-connected node, not an external client.
- **Player name is NOT in the envelope.** It's in the `/start` payload. Display tooling resolves `source_id → player_name` by reading the corresponding `/start` event from storage. Avoids leaking names on every event.

## Schema registry for airdefenders

Per [fabric-schema-evolution.md](fabric-schema-evolution.md). Files under `docs/design/schemas/airdefenders/`:

```
session.start.v1.schema.json
session.end.v1.schema.json
wave.start.v1.schema.json
wave.summary.v1.schema.json
emplacement.layout.v1.schema.json
radar.detect.v1.schema.json
radar.sweep.v1.schema.json
track.update.v1.schema.json
threat.assess.v1.schema.json
engagement.fire.v1.schema.json
engagement.terminate.v1.schema.json
impact.v1.schema.json
resupply.v1.schema.json
```

Example — `engagement.fire.v1.schema.json`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "schema://goat/airdefenders/engagement.fire.v1",
  "type": "object",
  "required": ["launcher_id", "mode", "target_track_id", "interceptor_id", "fire_xy"],
  "properties": {
    "launcher_id": { "type": "integer", "minimum": 0 },
    "mode": { "enum": ["PROX", "DIRECT", "AUTOPILOT"] },
    "target_track_id": { "type": "string" },
    "interceptor_id": { "type": "string" },
    "fire_xy": {
      "type": "array",
      "items": { "type": "number" },
      "minItems": 2, "maxItems": 2
    }
  }
}
```

## Stateful consumer — per-engagement Pk

Per [fabric-stateful-consumers.md](fabric-stateful-consumers.md). Declaration:

```go
consumer := stateful.New(stateful.Config{
    Name:            "airdefenders-engagement-correlator",
    CorrelationKey:  "payload.interceptor_id",
    OpenOn:          []string{"engagement/fire"},
    CloseOn:         []string{"engagement/terminate"},
    SessionTimeout:  30 * time.Second,
    MaxOpenSessions: 10_000,
    PersistencePath: "",  // memory-only; game telemetry loss tolerable
    DLQTopic:        "airdefenders/{site}/_dlq/orphaned-engagements",
})
```

On complete: compute `kill|miss|self_destruct` outcome, emit `engagement_pk_samples` metric tagged by `mode`, publish derived event `airdefenders/{site}/derived/engagement/pk/{id}` with the correlated pair.

On orphan: publish to DLQ with the original fire event + reason. Investigate post-hoc.

## Nightly analytics — population profile

Per [event-telemetry-schema.md §subscribers/batch](event-telemetry-schema.md#3-batch-analytics-over-history). Cron'd job:

```
Daily at 04:00 UTC (after traffic trough):
  Query airdefenders/*/session/*/wave/summary for last 24h
  Query airdefenders/*/session/*/derived/engagement/pk/* for last 24h
  Compute population vector:
    [pk_prox_mean, pk_prox_std, pk_direct_mean, pk_direct_std, pk_auto_mean, pk_auto_std,
     economy_prox, economy_direct, economy_auto,
     reaction_p50, reaction_p95, leaker_rate,
     emplacement_style_vector (4-dim)]
  Publish to airdefenders/{site}/population/profile/v1
  Publish per-player delta to airdefenders/{site}/player/{name_hash}/delta/v1
```

Population vector is small (<100 floats). Stored with 1-year retention. Per-player deltas have 30-day retention.

## Browser → Go → Zenoh data path

```
┌──────────────────┐
│  Browser         │  events emitted via TelemetryClient (ops/serve/common/telemetry.js)
│  game loop       │  batched 32 events or 500ms, whichever first
└────────┬─────────┘
         │ HTTPS POST /api/telemetry
         │ batch of N events with client-side envelope (ts_client_ns, ts_mono_ns, site_id=unset, seq)
         ▼
┌──────────────────┐
│  Go server       │  validates each event against registered schema
│  airdefenders    │  stamps ts_ntp_ns (from timehygiene.AttestedNow), site_id (server's), source_id (session)
│                  │  publishes to fabric via ops/telemetry-client
└────────┬─────────┘
         │ session.Put(keyexpr, envelope-wrapped payload)
         ▼
┌──────────────────┐
│  Zenoh router    │  storage plugin writes to RocksDB (30-day TTL, raw topics)
│                  │  federates to other sites over WireGuard
└────────┬─────────┘
         │
         ├──▶ zenoh-prom-exporter       — scrapes rates/counts for Grafana dashboard
         ├──▶ stateful consumer         — fire → terminate correlation, emits derived/pk
         ├──▶ nightly analytics job     — population profile rollup
         └──▶ audit-logger interceptor  — Rust, already running; records all `ops/**` — will be extended to `airdefenders/**`
```

## Browser offline behavior

The browser telemetry SDK buffers events in IndexedDB when the server is unreachable. On reconnect, the queue is drained in batches. Envelope `ts_client_ns` records when the event happened; `ts_ntp_ns` is stamped on server ingest using the time of ingest (not the time of origin), and the difference is tracked via the `ingest_delay_ns` field.

For replay fidelity, downstream consumers SHOULD use `ts_client_ns` for event ordering within a session and `ts_ntp_ns` for cross-session / cross-site alignment. The schema v1 registry captures this guidance.

## Security + privacy

- Player names are free-text (12 chars), UPPERCASE A-Z 0-9. Already validated server-side. Displayed in leaderboard as-is.
- Player names are NOT in envelopes — only in `/start` events. Display tooling resolves by session.
- For population analytics, player names are hashed (stable per-name hash, not per-session) to `name_hash` for the `/player/{name_hash}/delta` topic. Reverse-mapping requires the server's name→hash table, which is not on the fabric.
- Event payloads contain game state only. No credentials, no client IPs, no user-agent strings.

## Storage + retention

| Topic pattern | Retention | Rationale |
|---|---|---|
| `airdefenders/*/session/*/start`, `/end` | 30 days raw | Session-level auditing + analytics inputs |
| `airdefenders/*/session/*/wave/**` | 30 days raw | Per-wave replay + analytics inputs |
| `airdefenders/*/session/*/engagement/**` | 30 days raw | Pk computation inputs, replay |
| `airdefenders/*/session/*/radar/detect` | 14 days | Moderate volume; replay window shorter |
| `airdefenders/*/session/*/radar/sweep` | 3 days if enabled | Very high volume; replay-only |
| `airdefenders/*/session/*/track/update` | 3 days if enabled | Very high volume; replay-only |
| `airdefenders/*/derived/engagement/pk/*` | 90 days | Derived product; longer view for tuning |
| `airdefenders/*/population/profile/v1` | 1 year | Small, high-value historical view |
| `airdefenders/*/player/*/delta/v1` | 30 days | Rolling comparison; older deltas not useful |
| `airdefenders/*/_dlq/**` | 30 days | Orphan investigation window |

Retention is Zenoh storage TTL, configured per keyexpr prefix in `ops/ansible/roles/zenoh/templates/config.json5.j2`.

## What this costs the project vs what it produces

- **SQLite path stays alive** as a fallback. The Go server retains the SQLite write path alongside Zenoh publishing until Step 9 completes and the leaderboard read path has been migrated. Game keeps working throughout.
- **Retained for free:** all UI, all game logic, all mesh-only routing, all hardened-base build posture, all observability, all deployment tooling.
- **Added:** ~500 LOC of Go instrumentation (telemetry-client + ingest endpoint), ~100 LOC of JS instrumentation, ~13 JSON schema files, one Grafana dashboard, one stateful-consumer config, one analytics job. All of it is either reusable platform code OR thin configuration on top of reusable platform code.
- **Produced for the broader system:**
  - `ops/telemetry-client/` Go module exists — gatewatch migrates to it, future apps use it.
  - `ops/serve/common/telemetry.js` exists — emplace / shield-planner / future apps use it.
  - `ops/zenoh-prom-exporter/` exists — any future fabric-to-Prom bridge config, not code.
  - `ops/zenoh-consumer/skeleton/` exists — first stateful consumer pattern, directly applicable to TAK CoT track lifecycle + DDS bridge.
  - `ops/zenoh-analytics/skeleton/` exists — first batch analytics pattern, directly applicable to SLO rollups + health event clustering.
  - `docs/design/schemas/` exists — schema registry for the whole project.
  - `ops/schema-tools/` exists — validator + diff tool + migrator, CI-enforced.
  - First application-layer NTP attestation caller — Block 12 finally has a real app-layer consumer.
  - First real caller for admit-audit event-timestamp validation — Block 12B-sidecar finally has app-layer traffic.

## Open questions

- **High-rate topics (radar/sweep, track/update) gated behind feature flags.** Decision: default off, turn on only for replay-needed sessions. Revisit if storage TTL gets violated under load.
- **Archetype clustering in-game display (Step 9D).** Nice demo, uncertain UX value. Ship only if the preceding steps have produced useful data; otherwise defer indefinitely.
- **Cross-site leaderboard aggregation.** Today there's only one site with airdefenders running; no cross-site replication required. If multi-site deployment happens later, the keyexpr shape already supports it.
