# airdefenders-schemas

Canonical `.proto` data definitions for the four event topics that the
airdefenders multiplayer publisher emits on the nova Zenoh trial
fabric. Companion to
[`docs/design/airdefenders-multiplayer.md`](../../../docs/design/airdefenders-multiplayer.md)
and a sibling of
[`ops/zenoh-trial-peers/synth-vendor-schemas/`](../synth-vendor-schemas/).

These `.proto` files are **canon** for the airdefenders publisher's
data dictionary — they describe the field semantics of each event, are
compiled into FileDescriptorSets (`.fds`) and uploaded to the portal so
the schema viewer (`/schema.html?key=...`), the per-language autogen
code surface, and the inspector's "decode samples" path all light up.

## Layout

```
airdefenders-schemas/
├── proto-airdefenders/
│   ├── common.proto              # Vec2 (shared by every per-event proto)
│   ├── radar_detection.proto     # airdefenders.v1.RadarDetection
│   ├── missile_fired.proto       # airdefenders.v1.MissileFired
│   ├── missile_intercept.proto   # airdefenders.v1.MissileIntercept
│   └── shahed_impact.proto       # airdefenders.v1.ShahedImpact
├── topic-manifest.tsv     # topic ↔ proto ↔ message-name ↔ wire-format
├── build-fds.sh           # protoc → out/*.fds
├── upload-all.sh          # POST .fds to live portal (idempotent)
├── README.md              # this file
└── out/                   # generated FileDescriptorSet bodies (gitignored)
```

## Topics in scope

| Vendor | Key pattern (nova) | Message | Wire format |
|---|---|---|---|
| airdefenders | `airdefenders/nova/radar/detection/v1` | `airdefenders.v1.RadarDetection` | json |
| airdefenders | `airdefenders/nova/missile/fired/v1` | `airdefenders.v1.MissileFired` | json |
| airdefenders | `airdefenders/nova/missile/intercept/v1` | `airdefenders.v1.MissileIntercept` | json |
| airdefenders | `airdefenders/nova/shahed/impact/v1` | `airdefenders.v1.ShahedImpact` | json |

Source of truth for this list is
[`topic-manifest.tsv`](topic-manifest.tsv). The `<SITE>` placeholder in
the manifest is substituted by `$SITE_ID` (default `nova`) at upload
time so the same descriptor set works for nethost + nova + future sites.

## Building + uploading

```bash
# 0. protoc has to be on PATH. macOS: brew install protobuf.
#    Linux: apt install protobuf-compiler. Verify libprotoc 3.x+.

# 1. Compile every .proto to a FileDescriptorSet (.fds) under out/.
./build-fds.sh

# 2. Reconcile against the live portal. Idempotent: skips topics
#    whose v1 schema body already matches by sha256; leaves a SKIP
#    marker when content differs; emits MISS when the topic isn't
#    registered yet (run bootstrap-airdefenders-portal.sh first).
export PORTAL_URL=https://portal.nova.netbird.nova.example
export PORTAL_OPERATOR_API_KEY="$NOVA_PORTAL_OPERATOR_KEY"
./upload-all.sh

# 3. Light up the portal viewer + inspector decode by PATCHing the
#    topic.format from `json` → `protobuf:<MsgName>`. Idempotent.
./upload-all.sh --set-format
```

The portal exposes the `.fds` bytes at `/api/schemas/<key>` and the
structured walk at `/api/schemas/render/<key>` — the schema viewer
consumes the latter and renders a Swagger-style message tree, an
auto-derived `.proto` reconstruction, and per-language autogen code
examples for Rust/Go/Python/TypeScript.

## Wire-format gap (deliberate, matches synth-vendor)

The airdefenders Go publisher emits **JSON** on the wire — the
envelope's `payload` field is a `map[string]any` rendered by
`encoding/json`. The portal topic's `format` field reads
`protobuf:airdefenders.v1.<MsgName>` after `upload-all.sh --set-format`
runs.

This advertised-protobuf vs JSON-bytes mismatch is the same gap the
synth-vendor schemas live with — see
[CLAUDE.md](../../../CLAUDE.md) under "Notional collapse" /
"Synthetic vendor publishers". The portal's inspector "decode samples"
path will attempt to `dynamicpb`-decode the JSON bytes and fail
per-sample; the schema viewer + autogen-code surfaces are unaffected
because they walk the descriptor, not the wire.

Flipping the wire format to genuine protobuf is a future task; the
JSON payload is what the original single-player code consumed and the
canonical payload list in
[`docs/design/airdefenders-multiplayer.md`](../../../docs/design/airdefenders-multiplayer.md)
documents the JSON-on-wire shape.

## protoc is not bundled

This repo doesn't vendor `protoc`. The `.proto` sources + scripts are
checked in; the operator runs `build-fds.sh` once on first invocation
of `bootstrap-airdefenders-portal.sh` to generate the `.fds` bodies.
The bootstrap script calls `build-fds.sh` automatically when `protoc`
is on PATH and emits a clear "install protoc, then re-run" message
when not.

## Block 11 Step 9 cross-ref

NTP attestation is deliberately **not** modeled here — the envelope
shape in
[`docs/design/airdefenders-multiplayer.md`](../../../docs/design/airdefenders-multiplayer.md)
§"Envelope" has the comment line spelling out why. Step 9 introduces a
reusable `telemetry-client` Go module that all fabric publishers will
import; wiring `chronyc` into airdefenders here would build the exact
"airdefenders-only code" Step 9 was scoped to prevent. Re-enable when
Step 9 lands.

## Adding a new event topic

1. Drop a new `.proto` under `proto-airdefenders/` with
   `package airdefenders.v1;` and `import "common.proto";` if it uses
   `Vec2`.
2. Append a row to `topic-manifest.tsv`.
3. Register the topic in the portal (or add it to the `TOPICS=()`
   array in
   [`../bootstrap-airdefenders-portal.sh`](../bootstrap-airdefenders-portal.sh)
   so a single re-run picks it up).
4. Run `./build-fds.sh && ./upload-all.sh --set-format`.

## Field-number stability

Field numbers in these `.proto` files are stable for v1 — never
renumber. Adding new optional fields at higher numbers is safe.
Renaming a field at the same number is also wire-compatible for
protobuf, but the JSON wire format keys off field NAMES, so a rename
is a breaking change in this trial-fabric posture. Cut a `v2` topic
key (`airdefenders/<site>/<event>/v2`) for any breaking change.
