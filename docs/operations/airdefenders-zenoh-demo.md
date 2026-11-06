# Air Defenders → Zenoh fabric demo (runbook)

Eight auto-pilot bots play the co-op game in one browser tab; every radar
detection, missile launch, intercept and ground impact they cause is published
onto the goatnet's Zenoh fabric, where you watch it land. The point is the
**fabric**, not the gameplay: the bots are a traffic generator that drives a
continuous, varied stream of real events.

Runs on any goatnet that hosts airdefenders with the publisher on — zagros,
zagros-prod and zagros-dev today. Everything below is written for `<site>` =
the goatnet's `site_id` (`zagros`, `zagros-prod`, `zagros-dev`) and `<zone>` = its
mesh DNS zone (`netbird.zagros.example`, `netbird.zagros-prod.example`,
`netbird.zagros-dev.example`). Where a literal is easier to copy, zagros is used.

Design background: [`airdefenders-multiplayer.md`](../design/airdefenders-multiplayer.md)
§"Auto-pilot personae", §"Zenoh mapping", §"One-page demo". This page is the
operator runbook.

## The two-minute version

You need: a platform-admin login on the goatnet, a browser on its mesh, and
(for the terminal half) a `goat` context for it.

1. Open `https://airdefenders.<site>.<zone>/` and log in when the door asks.
2. On the title screen, press **▶ RUN FABRIC DEMO (admin)**. A new tab opens
   with eight bots; within about ten seconds they are in combat and publishing.
3. Watch the fabric. Pick any:
   - the tab's top bar: copy the `goat sub` command and run it in a terminal,
     or click **flashlight** / **panoscope** / **stethoscope** to open a
     surface already focused on airdefenders;
   - `goat demo airdefenders --no-open` in a terminal (tails without opening a
     second browser).
4. Close the demo tab to stop. Players drop to 0 and publishing stops.

The button appears only for platform admins. Everyone else can play at `/` and
`/mp` and never sees it; if they browse to `/demo` by hand they get a 403 that
says why.

## The other two ways in

All three paths put the same eight personae into the same room; pick by how you
want to watch.

**`goat demo airdefenders`** — one command from a terminal on the mesh with the
goatnet's context selected (`goat context use zagros`). It derives
`https://airdefenders.<site>.<zone>/demo` and `<site>` from the context's
portal endpoint, opens the page in your default browser (you log in there if
you have no session yet; the page needs the admin login above) and runs
`goat sub --literal 'airdefenders/<site>/**' --follow` in the terminal. Ctrl-C
stops the tail; close the tab to drop the bots. Needs goat-cli ≥ 0.5.4 (the
`--literal` flag); an older binary prints the tail command for you to run.

```bash
goat context use zagros
goat status                       # three green lines: ztol / portal / cert
goat demo airdefenders            # opens /demo + tails airdefenders/zagros/**
goat demo airdefenders --tui      # same, interactive Tail lens
goat demo airdefenders --no-watch # browser only
goat demo airdefenders --no-open  # tail only
```

**`spawn-bots.sh`** — eight separate Chrome windows in a grid, one persona
each, on throwaway profiles. macOS + Chrome only. Behind the authenticated
door each throwaway profile has to log in separately, so this path is now the
awkward one on a goatnet; it remains useful against a laptop server
(`AD_DEMO_UNGATED=1 go run .`), where there is no door.

```bash
ops/serve/airdefenders/spawn-bots.sh https://airdefenders.zagros.netbird.zagros.example
AD_BOTS="trigger nomad gremlin" ops/serve/airdefenders/spawn-bots.sh https://airdefenders.zagros.netbird.zagros.example
ops/serve/airdefenders/spawn-bots.sh --kill
```

## What flows

Eight personae (Trigger, Deadeye, Gremlin, Dozer, Sloppy, Squint, Nomad, Camel)
collectively exercise every player action, so a full fleet drives all four
published event types continuously:

| Key | Emitted when | Persona that drives it |
|---|---|---|
| `airdefenders/<site>/radar/detection/v1` | a radar sees a threat | Squint (only engages what radar sees) |
| `airdefenders/<site>/missile/fired/v1` | a launcher fires | Trigger / Nomad (spray) |
| `airdefenders/<site>/missile/intercept/v1` | an interceptor kills a shahed | Deadeye (high hit ratio) |
| `airdefenders/<site>/shahed/impact/v1` | a threat leaks to the ground | Dozer / Gremlin (let things through) |

Each event is a JSON envelope + payload:

```
airdefenders/zagros/missile/intercept/v1 {"envelope":{"site_id":"zagros","event_type":"missile.intercept",
  "ts_iso":"2026-09-11T14:02:10.195Z","schema_version":"v1",
  "source":{"service":"airdefenders","instance":"<mgmt-host>","room_id":"global"}},
  "payload":{"interceptor_eid":305,"kill_pos":{"x":570.9,"y":301.4},"player_id":15,"shahed_eid":303,"wave":1}}
```

The persona mix shapes the pattern you watch: a fired-heavy stream from
Trigger, clean fired→intercept correlation from Deadeye, `shahed/impact`
leaks from the sloppy ones.

## Watching: the four surfaces

The four surfaces answer four distinct questions about the same fabric. The
demo tab's top bar links the three web ones already focused on airdefenders.

**`goat sub` — the most direct witness (terminal).**

```bash
goat sub --literal 'airdefenders/zagros/**' --follow      # line-oriented live tail
goat sub --literal 'airdefenders/zagros/**' --tui         # event table + rate sparkline
goat sub --literal 'airdefenders/zagros/**' --count 20    # capture 20 events then exit
```

`--literal` matters on the zagros goatnets: they run the slot-pool identity
model, where `goat sub` treats the first key segment as a slot alias and
refuses an unknown one. `airdefenders/` is a seeded vendor prefix beside the
pool, not a slot, so the flag says "this key is exactly what I mean".
`goat demo` sets it for you (goat-cli ≥ 0.5.4).

Seeing events here is also the definitive receipt: the router's ACL admitted
the writes and a real subscriber received them. Ingress denial is silent
otherwise, so an empty tail while the bots are fighting means the
sidecar's identity is not being admitted — see Troubleshooting.

**flashlight — focused live tail (web).**
`https://flashlight.<site>.<zone>/?key=airdefenders/<site>/**`
What is happening in this slice right now.

**panoscope — whole-fabric picture (web).**
`https://panoscope.<site>.<zone>/?key=airdefenders/<site>/missile/fired/v1`
The game's traffic in context with everything else on the fabric, with the
airdefenders publisher selected and its side panel streaming. Use the `key=`
form: on the slot-pool goatnets the publisher carries no vendor tag, so
`?vendor=airdefenders` selects nothing.

**stethoscope — per-target verdict (web).**
`https://stethoscope.<site>.<zone>/?topic=airdefenders/<site>/**`
Publish freshness and schema conformance for the airdefenders topics.

Deep-link parameters follow
[`zenoh-surface-deep-links.md`](../design/zenoh-surface-deep-links.md). The
surfaces sit behind the same kind of authenticated door as the game, so the
first one you open may bounce through a silent login redirect.

## Quantitative proof

The game counts every publish, labelled by event type, at `/metrics`. That
path skips the door (the Prometheus blackbox job probes it), so a plain curl
works from any mesh peer:

```bash
curl -s https://airdefenders.zagros.netbird.zagros.example/metrics | grep -E 'zenoh_publish|mp_players'
# airdefenders_mp_players 8
# airdefenders_mp_zenoh_publish_errors_total 0
# airdefenders_mp_zenoh_publish_total{event_type="missile.fired"} 140
# airdefenders_mp_zenoh_publish_total{event_type="radar.detection"} 68
# airdefenders_mp_zenoh_publish_total{event_type="missile.intercept"} 48
# airdefenders_mp_zenoh_publish_total{event_type="shahed.impact"} 38
```

Run it a few seconds apart: the counters climb while a wave is in progress and
`..._errors_total` stays at 0. Counters reset when the container restarts.

## How it works (one paragraph)

The Go game server cannot open a native Zenoh session, so it publishes each
event as a plain HTTP PUT to the **rest-pub-bridge** sidecar in the same
compose stack, which holds the per-vendor X.509 leaf
(`CN=airdefenders-trial-peer`) and re-publishes over a native mTLS session.
The router's access control admits writes to `airdefenders/**` because the
`airdefenders` vendor is in the goatnet's seed-vendor list; an identity-less
REST publish would be dropped silently at ingress. Subscribers — your
`goat sub`, the surfaces, the audit subscriber — then receive the samples. The
`/demo` page itself is eight same-origin iframes of `/mp?bot=<persona>`; the
server cannot tell a bot from a person. Who may open it is decided by the
game from the door's forwarded identity
([`demo_auth.go`](../../ops/serve/airdefenders/demo_auth.go)). Detail:
[`airdefenders-multiplayer.md`](../design/airdefenders-multiplayer.md)
§"Zenoh mapping" + [`zenoh-rest-pub-bridge.md`](../design/zenoh-rest-pub-bridge.md).

## Performance: the lag is the link

Each of the eight frames pulls a 30 Hz full-state stream from the mgmt host.
Over a relay-routed mesh hop that fans about 1.5 MB/s into one laptop and the
renders can stutter. That is the laptop↔host link, not the game: the server is
authoritative, and the fabric gets every event regardless. For a smoother
picture run the demo tab on a machine close to the host, or watch the fabric
rather than the frames.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| No DEMO button on the title screen | you are not in `PLATFORM-ADMIN` on this goatnet, or the door is off for this deployment | check `goat who` for your groups; `/api/me` returns `{"demo_admin":false}` — an admin sees `true` |
| `/demo` answers 403 "only available behind the authenticated door" | `airdefenders_oauth2_proxy_enabled` is false on this goatnet, so the game trusts no identity | turn the door on (`authenticated-doors.yml`) or run locally with `AD_DEMO_UNGATED=1` |
| Bots fight, metrics climb, but `goat sub` shows nothing | your tail key does not match the game's `SITE_ID` (the container env) | `docker inspect airdefenders` on the host and compare `SITE_ID` with `<site>`; the group_vars derive it from `site_id` |
| `goat sub` refuses: "no slot aliased `airdefenders`" | slot-pool goatnet; the resolver read the vendor prefix as a slot alias | add `--literal` (goat-cli ≥ 0.5.4) |
| Metrics climb, `publish_errors_total` is 0, still nothing on the fabric | the sidecar's leaf is not admitted by the router (ACL, or a CA re-issue left the leaf behind) | `docker logs airdefenders-zenoh-sidecar`; re-mint via the bootstrap script below and redeploy |
| `publish_errors_total` climbing | the game cannot reach the sidecar | `docker ps` — the sidecar should be up on the `<zenoh_stack>_default` network |
| A 9th player is refused | the eight frames use the eight per-IP WebSocket slots | close a standalone game tab from the same machine, or set `AD_MAX_CONNS_PER_IP` |
| `400 Request Header Or Cookie Too Large` | a stale door cookie jar | clear cookies for the airdefenders host |

## Deploy / refresh

Only needed if the game is down, on a stale image, or a group_var changed.
From a checkout whose `airdefenders_image` pin is current, with the goatnet
reachable ([env doors](../design/env-doors.md) for a goatnet your laptop is
not meshed to):

```bash
cd ops/ansible
OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES aws-vault exec usareur-af --no-session -- \
  ../scripts/ansible-preflight.sh \
  -i inventory/mgmt-hosts.ini -i inventory/mgmt-hosts-mesh.ini \
  -e target=zagros_hosts \
  airdefenders-deploy.yml
```

`-e target=zagros_prod_hosts` / `zagros_dev_hosts` for the others (zagros-prod
through its door, so `-i inventory/doors-groupvars.ini` instead of the mesh
inventory). The play pulls the pinned image, renders the compose with the
publisher and the demo gate wired, and brings up the game + sidecar. The door
itself is a separate play (`authenticated-doors.yml`) and does not need a
rerun for a game redeploy.

One-time per goatnet, before the first deploy with the publisher on: mint the
vendor and register the four topics in that goatnet's portal, then put the
leaf on the mgmt host:

```bash
PORTAL_URL=https://portal.zagros.netbird.zagros.example \
PORTAL_OPERATOR_API_KEY=... \
SITE_ID=zagros \
SCP_TARGET=goat@<mgmt-host mesh ip>:/opt/airdefenders/certs \
  ops/zenoh-trial-peers/bootstrap-airdefenders-portal.sh
```

The role pre-flight refuses the deploy if the leaf, key or CA root is missing
and names the path. A CA re-issue on the goatnet invalidates the leaf: re-run
the bootstrap, then redeploy.

Rollback is a pin revert: `ops/scripts/rollback.sh airdefenders 1`
(ADR 0859; ledger `ops/ansible/roles/airdefenders/deploy-history.yml`).
