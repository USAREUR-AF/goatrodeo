# Air Defenders — Epic Wrath (variant)

> **Status: design only.** Implementation gated on [Block 48E](../project/implementation-plan.md) green-light per CLAUDE.md substrate-gate ("no new cluster-captain assignments until 48E green-lights scale-out"). This doc + the [Block 67](../project/implementation-plan.md) row are draft-and-queue per path (a) of the cluster-planning brief 2026-05-05.

## Conclusion

Air Defenders Epic Wrath is a **clean fork** of [airdefenders-classic](airdefenders-leaderboard.md) — a multiplayer, async / turn-based, server-authoritative game whose **architectural purpose is to torture-test the goat substrate's multi-master + partition-tolerance + schema-evolution + wide-event surfaces** at richer dimensionality than Classic's single append-only leaderboard exercises.

Classic stays untouched. Epic Wrath is a new top-level service at [`ops/serve/airdefenders-epic-wrath/`](../../ops/serve/airdefenders-epic-wrath/) with its own schema, module tree, frontend, and lifecycle. **No shared library, no shared schema, no cross-variant identity import, no bug-fix back-port mechanism.** Goat-wide platform code (Zenoh client, Ory, OTel, base-image roles, schema-enforcer per [Block 23](ai-rapid-adaptation.md)) is the only shared layer; that's *platform*, not "airdefenders shared."

## Why this variant exists

Two co-acting design criteria, both load-bearing:

1. **Exercise goat at multi-master + partition + schema dimensions richly.** Classic's `scores` table is append-only with synthetic PK and `max()` resolution — it converges cleanly under F-003 LWW per-row by construction. That's the trivial case. Epic Wrath introduces **14 tables** spanning all four reconverge classes (append-only / single-writer-per-row / read-mostly admin-edit / concurrent-same-row), forcing every [ADR 1010](../adr/1010-ory-on-spock-mindful-operation.md)-style discipline (D1–D6) to surface against game-shaped data.
2. **Implementable by AI-agent teams.** Patterns agent-teams succeed at: server-authoritative discrete state, sharply-bounded modules, deterministic feedback loops, backend-heavy. Patterns they spin on (per the [Block 48 substrate research](multi-agent-team-management.md) §6.4 mainline-tenacity tells): real-time client physics, SPA state management, visual polish. Epic Wrath chooses against the latter on purpose.

The game itself is the test artifact. Operational signal flows back to: F-NNN findings on Spock behavior, M-NNN measurements on partition-reconverge time, validation-state.md rows on cross-cluster soak.

## Game shape (locked; not relitigated)

| Dimension | Choice | Rejected alternative | Why |
|---|---|---|---|
| **Tempo** | Async / turn-based | Real-time twitch | Discrete state transitions map cleanly to Spock replication tests; eliminates client-timing-dependent test signal |
| **Authority** | Server-authoritative match resolution | Client-trusted scoring | Forces cross-table atomic write surface (match + ladder + inventory) which is the load-bearing reconverge case |
| **Strategy depth** | Persistent (ranking, inventory, achievements carry across matches) | One-shot arcade | Enables multi-table schema → enables richer reconverge probes |
| **Frontend** | Server-rendered Go templates + htmx-style progressive enhancement | SPA (React / Vue / Svelte) | Backend-heavy split favors agent-team execution; visual polish out of scope |
| **Identity** | Ory-backed per [ADR 0411](../adr/0411-ory-idp-mvp.md) | Self-hosted bespoke auth | Inherits goat platform; no airdefenders-specific identity surface |
| **Schema** | New top-level prefix `epic_*` (not shared with Classic) | Schema reuse | F-004 + F-003 reconverge analysis is per-table; sharing creates cross-variant coupling we don't want |

## Module boundary table

Each module is **ownable by one agent** under [Block 48 substrate](multi-agent-team-management.md) Pattern A (track-direct-to-main). Contract-first: each module's interface doc lands before any code.

| Module | Path | Owns tables | RPC surface | Cross-module deps |
|---|---|---|---|---|
| **identity** | [`ops/serve/airdefenders-epic-wrath/identity/`](../../ops/serve/airdefenders-epic-wrath/identity/) | (none — Ory-backed) | `/login` `/logout` (redirects to Kratos) | none |
| **profile** | `ops/serve/airdefenders-epic-wrath/profile/` | `epic_profile` | `GET /profile/:id` `POST /profile/edit` | identity |
| **ranked-ladder** | `ops/serve/airdefenders-epic-wrath/ladder/` | `epic_rank_score` `epic_season` | `GET /ladder` `GET /ladder/season/:id` | profile |
| **match-engine** | `ops/serve/airdefenders-epic-wrath/match/` | `epic_match` `epic_match_move` `epic_match_history` | `POST /match/new` `GET /match/:id` `POST /match/:id/move` `POST /match/:id/resolve` (server-fired) | profile, ladder, inventory |
| **inventory** | `ops/serve/airdefenders-epic-wrath/inventory/` | `epic_inventory_item` `epic_item_definition` | `GET /inventory` `GET /shop` `POST /shop/buy` | profile |
| **achievements** | `ops/serve/airdefenders-epic-wrath/achievements/` | `epic_achievement_definition` `epic_achievement_grant` | `GET /achievements` (write-side fired by match-engine) | profile |
| **social-graph** | `ops/serve/airdefenders-epic-wrath/social/` | `epic_friendship` `epic_friend_request` | `GET /friends` `POST /friends/request` `POST /friends/accept` | profile |
| **chaos-harness** | `ops/serve/airdefenders-epic-wrath/chaos/` | (none — emits OTLP) | `/admin/chaos/*` (gated to admin role) | (consumes all tables read-only) |
| **telemetry** | `ops/serve/airdefenders-epic-wrath/telemetry/` | (none — emits OTLP) | (background sidecar) | (consumes all RPCs via OTel hook) |

**Concurrent ownability**: nine modules → up to nine concurrent worker tracks (subject to [Block 48F](../project/implementation-plan.md) scale-out ceiling). Cross-module deps are unidirectional and shallow — the dep graph is a DAG with `identity` at the root.

## Schema + Spock replication-set composition

Per F-003 (LWW per-row default) + F-004 (no-PK tables rejected from `repset_add_all_tables`). Every Epic Wrath table has a PRIMARY KEY by construction; the F-004 critical-table list extends to all `epic_*` tables.

| Table | PK | Reconverge class | Spock LWW behavior | Discipline | F-NNN watch |
|---|---|---|---|---|---|
| `epic_profile` | `(player_id)` | **Single-writer-per-row** | Benign — player owns own row; only their session writes | D1 active-passive VIP (intra-site) + D4 admin-API-only writes (inter-site) | F-003 |
| `epic_rank_score` | `(player_id, season_id)` | **Concurrent-same-row possible** during partition reconverge (player plays a match at site A *and* site B during partition; both compute deltas) | **Dangerous** — LWW silently drops one site's delta | App-layer conflict resolution: match-engine writes `delta_event_log` (append-only) + nightly reconciliation recomputes ladder from event log; `epic_rank_score` is a *materialized cache* not the source-of-truth | F-003 + new F-NNN at landing |
| `epic_season` | `(season_id)` | **Read-mostly admin-edit** | Benign under D4 (admin-API-only writes) | D4 + D5 ACE drift-watch | F-003 |
| `epic_match` | `(match_id)` | **Single-writer-per-row** | Benign — match assigned a *home site* on creation; only home site writes | App-layer home-site invariant + D5 ACE drift-watch | F-003 |
| `epic_match_move` | `(match_id, move_seq)` | **Append-only** | Benign — synthetic PK, no UPDATE path | (none beyond F-004 PK requirement) | F-004 |
| `epic_match_history` | `(match_id)` | **Append-only at match-resolve time** | Benign — single write per row, never updated | (none) | F-004 |
| `epic_inventory_item` | `(player_id, item_id)` | **Single-writer-per-row** under match-resolve atomicity | Benign — only match-resolve OR shop-buy writes; serialized through match-engine | App-layer single-writer + D5 | F-003 |
| `epic_item_definition` | `(item_id)` | **Read-mostly admin-edit** | Benign under D4 | D4 + D5 | F-003 |
| `epic_achievement_definition` | `(achievement_id)` | **Read-mostly admin-edit** | Benign under D4 | D4 + D5 | F-003 |
| `epic_achievement_grant` | `(player_id, achievement_id)` | **Append-only** (achievement granted exactly once per player) | Benign — UPSERT with `ON CONFLICT DO NOTHING` | (none beyond F-004 PK requirement) | F-004 |
| `epic_friendship` | `(player_a_id, player_b_id)` (sorted) | **Concurrent-same-row possible** during partition (both players accept each other simultaneously at different sites) | **Dangerous-ish** — LWW silently drops one side's `accepted_at` | App-layer idempotent UPSERT + D5; both sides accepting is benign because the row's only state is "accepted" → no real conflict | F-003 |
| `epic_friend_request` | `(request_id)` | **Single-writer-per-row** with state machine (`pending → accepted/rejected`) | **Dangerous** — LWW silently drops state transitions during partition (request accepted at A, rejected at B) | App-layer state-transition log table `epic_friend_request_event` (append-only) + reconciliation rules ("accepted wins over rejected if same request_id"); the row is materialized cache | F-003 + new F-NNN |
| `epic_match_delta_event_log` | `(event_id)` | **Append-only** (source-of-truth for ladder reconciliation) | Benign | (none beyond PK) | F-004 |
| `epic_friend_request_event` | `(event_id)` | **Append-only** (source-of-truth for friend_request reconciliation) | Benign | (none beyond PK) | F-004 |

**Two new event-log tables (`epic_match_delta_event_log` + `epic_friend_request_event`) are the load-bearing reconciliation primitive.** Materialized caches (`epic_rank_score`, `epic_friend_request`) are derived from append-only logs via a reconciliation worker; under partition, both sites' caches diverge but their event logs converge cleanly. Pattern is structurally identical to event-sourced CQRS — but adopted here for the Spock reconverge property, not for the event-sourcing aesthetic.

## Partition-reconverge disciplines (mirrored from ADR 1010 D1–D6)

Each Epic Wrath table inherits the relevant subset:

- **D1 active-passive VIP intra-site** — universal (no Epic Wrath-specific surface).
- **D2 lifespan budgets** — match flow `lifespan ≥ 60 s × 10 = 10 min` so partition-reconverge windows don't strand mid-match writes; turn-timeout 24 h (game design, exceeds any realistic partition).
- **D3 service-side bounded retry** — match-engine handles `match-not-found` on cross-site requests via 3-attempt retry per ADR 1010 D3.
- **D4 admin-API-only mutations** — applies to `epic_season`, `epic_item_definition`, `epic_achievement_definition`. No direct SQL writes, ever.
- **D5 ACE drift-watch** — extends to all 14 `epic_*` tables. Materialized-cache divergence (`epic_rank_score`, `epic_friend_request`) reconciles via the event-log workers; ACE drift-watch on **event log tables** is the safety net.
- **D6 soak coverage** — per [ADR 1008](../adr/1008-soak-testing-discipline.md) discipline, soak driver lands at [`ops/serve/airdefenders-epic-wrath/tests/soak/`](../../ops/serve/airdefenders-epic-wrath/tests/soak/) covering: (a) two-site concurrent-match-resolve A16-shape probe; (b) partition-reconverge for materialized caches; (c) friend-request state-transition reconciliation; (d) admin-API-only invariant probe.

## Match-engine RPC shape (server-authoritative)

The match-engine is the **load-bearing example of cross-table atomic write under partition** — the test artifact this whole variant exists to exercise.

| Endpoint | Method | Authority | Cross-table writes | Reconverge surface |
|---|---|---|---|---|
| `/match/new` | POST | Server validates inventory + matchmaking | `epic_match` (insert) | Single-writer (home site) |
| `/match/:id/move` | POST | Server validates move legality, writes move | `epic_match_move` (append) | Append-only |
| `/match/:id/resolve` | POST (server-fired on match-end OR turn-timeout) | Server computes outcome + ladder deltas + inventory rewards + achievement grants | `epic_match_history` (append) + `epic_match_delta_event_log` (append) + `epic_inventory_item` (UPSERT under home-site invariant) + `epic_achievement_grant` (UPSERT idempotent) | **Atomic across 4 tables**; if home-site partitions mid-resolve, partial state must be safely re-runnable on other site |

**Atomic write under partition**: match-resolve writes are wrapped in a single PG transaction (intra-site atomicity) PLUS an idempotency key on `epic_match_delta_event_log.event_id = SHA256(match_id || resolve_attempt_seq)` so cross-site re-run produces the same row (D4 admin-API-only equivalent: re-run is safe by construction). The materialized `epic_rank_score` cache lags behind `epic_match_delta_event_log`; reconciliation worker runs every 30 s and on partition-recovery.

## Frontend approach

Server-rendered Go using stdlib `html/template` + `net/http`. **No SPA, no build step, no JS framework dep.** Progressive enhancement via [htmx](https://htmx.org/) attributes (`hx-get`, `hx-post`, `hx-swap`) for partial updates without full page reloads. Vanilla JS only for: (a) confetti/animation polish on match-resolve celebration, (b) optional WebSocket subscribe to `/match/:id/events` for live "opponent moved" notifications.

This frontend model is **structurally similar to airdefenders-classic** ([`ops/serve/airdefenders/index.html`](../../ops/serve/airdefenders/) embedded HTML pattern) but layered: instead of one embedded HTML file, Epic Wrath ships per-route Go templates colocated in each module dir.

**Rationale**: agent-teams produce predictable output for server-rendered HTML + htmx; SPA state-management is a known spin axis (per Block 48 §6.4 mainline-tenacity tells). Visual polish is explicitly out of scope; the game's appeal is mechanical depth, not animation.

## Wide-event emission (Block 27 mapping)

Per [`docs/design/observability-wide-events.md`](observability-wide-events.md). All Epic Wrath RPCs emit OTLP spans tagged with:

- `correlation_id` — propagated via `traceparent` header
- `player_id` — from Ory session
- `match_id` — when applicable
- `epic_module` — emitter module name (`match-engine`, `social`, etc.)

**Three named wide events** (in addition to per-RPC spans):

| Event name | Emitter | Trigger | Payload |
|---|---|---|---|
| `epic.match.resolved` | match-engine | After match-resolve transaction commits | match_id, outcome, both player_ids, ladder_deltas[], inventory_deltas[], achievement_grants[] |
| `epic.partition.reconciliation` | reconciliation worker | When materialized-cache reconcile from event-log produces a delta | table_name, divergence_count, source_event_log, reconcile_duration_ms |
| `epic.invariant.violated` | chaos-harness | When a runnable invariant assertion fails | invariant_name, expected_state, observed_state |

These three events feed [Block 27E ADR-promotion gate](../project/implementation-plan.md) (the 3-incident-reconstruction criterion) — Epic Wrath is a **producer of incidents** by design.

## Chaos-probe namespace (Block 38 mapping)

Per [`docs/design/continuous-architectural-chaos.md`](continuous-architectural-chaos.md). Every load-bearing invariant in this doc becomes a runnable assertion in [`ops/serve/airdefenders-epic-wrath/chaos/probes/`](../../ops/serve/airdefenders-epic-wrath/chaos/probes/). Initial probe set:

| Probe | Invariant | Failure mode it catches |
|---|---|---|
| `match_resolve_atomicity` | A successful `/match/:id/resolve` writes all 4 tables OR none | Partial-write under crash |
| `home_site_single_writer` | `epic_match` rows are written only by their home site | Cross-site bypass (operational discipline failure) |
| `materialized_cache_consistency` | `epic_rank_score` matches `SUM(delta) FROM epic_match_delta_event_log` per (player, season) | Reconciliation worker stalled |
| `friend_request_state_machine` | `epic_friend_request.state` transitions are monotone (`pending → {accepted, rejected}`, never reverse) | LWW dropped a state transition |
| `admin_api_only_writes` | No `epic_season` / `epic_item_definition` / `epic_achievement_definition` modification outside Kratos-authenticated admin path | D4 invariant breach via direct SQL |
| `epic_repset_pk_present` | Every `epic_*` table has a PRIMARY KEY | Schema migration introduces no-PK table |
| `event_log_append_only` | No UPDATE / DELETE on `epic_match_delta_event_log` or `epic_friend_request_event` | Operational discipline breach |

Probe failures emit `epic.invariant.violated` wide event AND draft an F-NNN finding entry per [ADR 1011](../adr/1011-findings-register.md).

## Schema evolution + migration discipline

Per [Block 23 mandatory schema declaration](ai-rapid-adaptation.md). Every `epic_*` table has a registered schema at `attest/schema/epic/<table_name>` consumed by the [schema-enforcer interceptor](data-dictionary.md). Migrations live at [`ops/serve/airdefenders-epic-wrath/migrations/`](../../ops/serve/airdefenders-epic-wrath/migrations/) using forward-only versioned SQL files.

**Per-cluster migration discipline** (analogue to F-006 postgres-spock init-script post-init restart): migrations run on each site independently; the migration tracker table itself is **excluded** from Spock replication (F-004 friendly: tracker table has PK; we explicitly skip via per-table loop, not bulk add).

Schema-evolution probes (Block 38 + Block 23): rolling-deploy probe verifies new-schema-aware code at site A interoperates with old-schema code at site B during a deploy window. A new `epic_*` column is acceptable; a removed column requires the standard expand-contract pattern.

## Build / deploy / observability inheritance

Inherits from goat platform (no Epic Wrath-specific surface):

| Concern | Source |
|---|---|
| Build (multi-stage on the base-image roles, static binary) | Mirrors [`ops/serve/airdefenders/Dockerfile`](../../ops/serve/airdefenders/Dockerfile) pattern |
| Deploy (Ansible role + per-site compose overlay) | Mirrors [`ops/ansible/airdefenders.yml`](../../ops/ansible/airdefenders.yml) pattern |
| Observability (Prometheus scrape + Grafana dashboard + isitup tile + smoketest entry) | Mirrors Classic |
| Identity | Ory per [ADR 0411](../adr/0411-ory-idp-mvp.md); group `epic-players` (regular) + `epic-admins` (D4 admin-API gate) |
| Audit | Block 17D integrity tier — every admin-API write writes a tamper-evident audit entry |
| TLS / mesh-only | Traefik + ip-allowlist 100.64.0.0/10 (per Classic) |

**No new platform commitments.** If Epic Wrath surfaces a need for a new platform pattern (e.g., a sharded-event-log primitive), that pattern lands in goat-core, not in Epic Wrath.

## Out of scope / non-goals

- **Real-time multiplayer** (twitch / WebSocket-driven game-state). Server-authoritative async only.
- **Visual polish** (3D, animations beyond confetti, sound design).
- **Microtransactions / real-money economy**. Inventory is gameplay-earned only.
- **Bot players / matchmaking AI**. Matchmaking is rank-bracket Glicko-style only; no AI opponents.
- **Cross-variant identity / inventory / leaderboard with Classic**. Clean fork = clean fork.
- **Anti-cheat beyond server-authoritative writes**. The mesh-only ipallowlist + server-side validation is the trust boundary; no client-attestation.
- **Federation with non-goat partner systems**. Partner-release patterns ([Block 28 WASM](wasm-conditioning-sandbox.md), [`mission-partner-sharing.md`](mission-partner-sharing.md)) do not apply — Epic Wrath is internal-fabric only.
- **CMVP-validated FIPS implementation**. Inherits goat's compliance posture; adds no FIPS surface.

## Open questions (for the captain agent at cluster spawn)

1. **Glicko-2 vs Elo for ladder math?** Glicko-2 has confidence intervals that interact better with partition-reconverge (a player's rank deviation grows during partition; reconciliation can use this). Default: Glicko-2; revisit if reconciliation gets messy.
2. **Match resolution timeout**: 24 h (long), 1 h (short), or per-match-mode? Default: 24 h, single fixed value, until evidence says otherwise.
3. **Reconciliation worker location**: per-site (each site reconciles its own materialized caches) vs. centralized (one site is canonical). Default: per-site, with ACE drift-watch on event-log tables as cross-site sanity check.
4. **Inventory item rarity tiers**: how many? Default: 4 (common/uncommon/rare/legendary); can adjust without schema change since rarity is a column on `epic_item_definition`.
5. **Soak driver harness**: extend [`run-2node-spock.sh`](../../ops/ansible/roles/ory/tests/soak/run-2node-spock.sh) pattern, or new shape? Default: extend (the pattern is proven, ADR 1008 says extend before fork).

## Companion artifacts

| Artifact | Path |
|---|---|
| Implementation plan section | [`docs/project/implementation-plan.md`](../project/implementation-plan.md) Block 67 |
| Classic variant (untouched) | [`docs/design/airdefenders-leaderboard.md`](airdefenders-leaderboard.md) |
| Classic telemetry (Block 11 Step 9) | [`docs/design/airdefenders-telemetry.md`](airdefenders-telemetry.md) |
| Substrate gate | [Block 48E](../project/implementation-plan.md) green-light |
| Cluster-kickoff template | [`docs/project/cluster-kickoffs/TEMPLATE.md`](../project/cluster-kickoffs/TEMPLATE.md) (instantiated post-48E) |
| Reference cluster shape | [`docs/project/cluster-kickoffs/security-ascent.md`](../project/cluster-kickoffs/security-ascent.md) (sister cluster) |
| Anchoring ADR | [ADR 1010](../adr/1010-ory-on-spock-mindful-operation.md) (D1–D6 disciplines applied per-table here) |
| Replication-set findings | F-003, F-004 |
| Schema-declaration discipline | [`docs/design/data-dictionary.md`](data-dictionary.md) ([Block 23](ai-rapid-adaptation.md)) |
| Wide-event integration | [`docs/design/observability-wide-events.md`](observability-wide-events.md) ([Block 27](../project/implementation-plan.md)) |
| Chaos-probe integration | [`docs/design/continuous-architectural-chaos.md`](continuous-architectural-chaos.md) ([Block 38](../project/implementation-plan.md)) |
