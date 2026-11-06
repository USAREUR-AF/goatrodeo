#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# One-shot bootstrap for the airdefenders multiplayer publisher on the
# Zenoh trial fabric. Mirrors the
# bootstrap-portal-vendors.sh + bootstrap-notional-portal.sh pattern:
#
#   1. POST /api/vendors  — mints the `airdefenders` vendor + cert
#      tarball + one-time portal api_key. The cert carries
#      `urn:goat-nova:vendor:airdefenders` as the URI-SAN (the portal's
#      minter handles the URI-SAN format).
#   2. Downloads + unpacks the cert tarball locally; optional SCP to
#      the mgmt host (so the operator doesn't have to schlep it by hand).
#   3. POST /api/vendors/airdefenders/topics  ×4 — registers each of
#      the four game-event topics with format=json initially. Existing
#      rows are PATCH'd so a re-run reconciles to the manifest exactly
#      (mirrors the notional bootstrap script's upsert behaviour).
#   4. Build + upload protobuf FileDescriptorSets (`.fds`) for each
#      topic and PATCH topic.format from `json` → `protobuf:<MsgName>`
#      so the portal's schema viewer (/schema.html) + per-language
#      autogen-code surfaces + inspector "decode samples" path all
#      light up. The wire format stays JSON (envelope.payload is still
#      a JSON object emitted by ops/serve/airdefenders/zenoh.go) — same
#      wire-format gap the synth-vendor schemas live with; see
#      ops/zenoh-trial-peers/airdefenders-schemas/README.md and
#      CLAUDE.md §"Notional collapse".
#
# Vendor-namespace note: `airdefenders` is the REAL-APP vendor (vs the
# synthetic-data vendors that follow the esoteric-animal naming
# convention per Block 82R). It's not a synth vendor — the portal's
# `synthetic` flag is set false here. See
# docs/design/airdefenders-multiplayer.md §"Vendor-namespace note".
#
# Topic registrations land in the portal so the Topics page lights up,
# panoscope shows the four key-expressions in its DAG, and an operator
# can drop into the inspector to verify game-event flow.
#
# Required env vars (sourced from .envrc.local.d/30-site-services.envrc):
#   PORTAL_URL                  — https://portal.<site>.<zone> (the goatnet's portal)
#   PORTAL_OPERATOR_API_KEY     — operator super-user key (one per portal)
#
# Optional env vars (defaults shown):
#   AIRDEFENDERS_VENDOR_CERT_OUT_DIR  ./airdefenders-vendor-certs
#   SCP_TARGET                        (empty = skip SCP) e.g.
#                                     <user>@<mgmt-host mesh ip>:/opt/airdefenders/certs
#   SITE_ID                           REQUIRED — the goatnet's site_id (for example site-a)
#
# Idempotency: re-runnable. A previously-minted vendor returns 409 (we
# swallow + skip the cert-mint); topics are PATCH'd when present, POST'd
# when not.

set -euo pipefail

: "${PORTAL_URL:?set PORTAL_URL e.g. https://portal.<site>.<zone>}"
: "${PORTAL_OPERATOR_API_KEY:?set PORTAL_OPERATOR_API_KEY (matches portal compose env)}"
PORTAL_URL="${PORTAL_URL%/}"
: "${SITE_ID:?set SITE_ID to the goatnet site_id (zagros, zagros-prod, ...) — it is the second key segment, airdefenders/<site>/**}"
SITE_ID="${SITE_ID}"
CERT_OUT_DIR="${AIRDEFENDERS_VENDOR_CERT_OUT_DIR:-./airdefenders-vendor-certs}"
SCP_TARGET="${SCP_TARGET:-}"

VENDOR="airdefenders"
DISPLAY_NAME="Air Defenders (multiplayer game-event publisher)"

# Topic catalog — four event-types per docs/design/airdefenders-multiplayer.md
# §Zenoh mapping. Format JSON at topic-create time; the schema-upload +
# format-PATCH stage at the end of this script flips them to
# `protobuf:<MsgName>` once the .fds bodies are uploaded.
# schema_ref points at the canonical .proto under
# ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/<topic>.proto;
# the design doc anchor is included after a `#` so operators can jump
# straight to either surface from the Topics page.
#
# When Block 11 Step 9 schema-registry work lands a richer envelope,
# this list does not change — the schema_ref column already points at
# the right source-of-truth.
#
# Field shape:   key_pattern | format | schema_ref | description | msg_name
TOPICS=(
  "airdefenders/${SITE_ID}/radar/detection/v1|json|ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/radar_detection.proto#airdefenders-multiplayer.md-radardetection|shahed detected by radar (transitions from visible==0 to visible>0)|airdefenders.v1.RadarDetection"
  "airdefenders/${SITE_ID}/missile/fired/v1|json|ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/missile_fired.proto#airdefenders-multiplayer.md-missilefired|interceptor launched from a launcher (post-arming for armed/autopilot modes)|airdefenders.v1.MissileFired"
  "airdefenders/${SITE_ID}/missile/intercept/v1|json|ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/missile_intercept.proto#airdefenders-multiplayer.md-missileintercept|shahed intercepted by interceptor proximity fuse (one event per shahed killed)|airdefenders.v1.MissileIntercept"
  "airdefenders/${SITE_ID}/shahed/impact/v1|json|ops/zenoh-trial-peers/airdefenders-schemas/proto-airdefenders/shahed_impact.proto#airdefenders-multiplayer.md-shahedimpact|shahed reached ground (leaker; lives decremented)|airdefenders.v1.ShahedImpact"
)

# api METHOD PATH [BODY]
# Writes body to stdout, HTTP_STATUS=<n> to stderr. Returns 0 on 2xx, 1 otherwise.
api() {
  local method="$1" path="$2" body="${3:-}"
  local tmp; tmp=$(mktemp)
  local code
  if [ -n "$body" ]; then
    code=$(curl -sk -o "$tmp" -w "%{http_code}" -X "$method" \
      -H "Authorization: Bearer ${PORTAL_OPERATOR_API_KEY}" \
      -H "Content-Type: application/json" \
      -d "$body" \
      "${PORTAL_URL}${path}")
  else
    code=$(curl -sk -o "$tmp" -w "%{http_code}" -X "$method" \
      -H "Authorization: Bearer ${PORTAL_OPERATOR_API_KEY}" \
      "${PORTAL_URL}${path}")
  fi
  cat "$tmp"; rm -f "$tmp"
  printf "HTTP_STATUS=%s\n" "$code" >&2
  case "$code" in 2*) return 0 ;; *) return 1 ;; esac
}

extract_status() {
  printf '%s\n' "$1" | awk -F= '/^HTTP_STATUS=/{s=$2} END{print s}'
}

mkdir -p "$CERT_OUT_DIR"

# -- Step 1: mint the vendor + cert tarball + one-time api_key -----------
echo "== creating vendor '${VENDOR}' =="
VENDOR_BUNDLE_URL=""
VENDOR_API_KEY=""

body=$(jq -nc \
  --arg p "$VENDOR" \
  --arg d "$DISPLAY_NAME" \
  --argjson aa true \
  --argjson syn false \
  '{prefix:$p, display_name:$d, auto_approve:$aa, synthetic:$syn}')

err_tmp=$(mktemp)
if resp=$(api POST "/api/vendors" "$body" 2>"$err_tmp"); then
  VENDOR_API_KEY=$(echo "$resp" | jq -r '.api_key')
  VENDOR_BUNDLE_URL=$(echo "$resp" | jq -r '.bundle_url')
  echo "  created."
  echo "  one-time api_key:  ${VENDOR_API_KEY}"
  echo "  one-time bundle:   ${VENDOR_BUNDLE_URL}"
  echo "  ↑ capture both immediately. The bundle is single-use; the api_key"
  echo "    is bcrypt-hashed server-side and cannot be retrieved."
else
  status=$(extract_status "$(<"$err_tmp")")
  case "$status" in
    409)
      echo "  already registered (HTTP 409). Skipping cert mint."
      echo "  If you need a fresh cert tarball, DELETE the vendor first via"
      echo "  POST /api/vendors/${VENDOR}/retire (requires retiring all"
      echo "  topics first) and re-run this script."
      ;;
    *)
      echo "  FAILED (HTTP ${status:-?}): $resp"
      echo "  aborting — topics under <${VENDOR}/**> won't be ACL-allowed."
      rm -f "$err_tmp"
      exit 1
      ;;
  esac
fi
rm -f "$err_tmp"

# -- Step 2: download + unpack the cert tarball when freshly minted ------
if [ -n "$VENDOR_BUNDLE_URL" ]; then
  echo
  echo "== downloading + unpacking cert tarball =="
  case "$VENDOR_BUNDLE_URL" in
    http*) full_url="$VENDOR_BUNDLE_URL" ;;
    /*)    full_url="${PORTAL_URL}${VENDOR_BUNDLE_URL}" ;;
    *)     full_url="${PORTAL_URL}/${VENDOR_BUNDLE_URL}" ;;
  esac
  mkdir -p "$CERT_OUT_DIR/$VENDOR"
  curl -fsS "$full_url" -o "$CERT_OUT_DIR/${VENDOR}.tar.gz"
  tar -xzf "$CERT_OUT_DIR/${VENDOR}.tar.gz" -C "$CERT_OUT_DIR/$VENDOR"
  echo "  unpacked into $CERT_OUT_DIR/$VENDOR/:"
  ls "$CERT_OUT_DIR/$VENDOR/"

  # The compose mount expects exact filenames. Rename if the tarball used
  # different conventions. Portal currently mints
  # `<prefix>-cert.pem` + `<prefix>-key.pem`; double-check then mv.
  if [ -f "$CERT_OUT_DIR/$VENDOR/${VENDOR}-cert.pem" ]; then
    echo "  cert filename matches Ansible expectation."
  else
    # Best-effort rename for common alternative shapes.
    for f in "$CERT_OUT_DIR/$VENDOR"/*.pem; do
      case "$(basename "$f")" in
        cert.pem|leaf.pem) mv "$f" "$CERT_OUT_DIR/$VENDOR/${VENDOR}-cert.pem" ;;
        key.pem|leaf-key.pem|private.pem) mv "$f" "$CERT_OUT_DIR/$VENDOR/${VENDOR}-key.pem" ;;
      esac
    done
    echo "  normalised filenames to ${VENDOR}-cert.pem + ${VENDOR}-key.pem"
  fi

  if [ -n "$SCP_TARGET" ]; then
    echo
    echo "== SCPing cert tree to $SCP_TARGET =="
    ssh "${SCP_TARGET%%:*}" "sudo mkdir -p ${SCP_TARGET#*:} && sudo chown -R \$USER ${SCP_TARGET#*:}" || true
    scp "$CERT_OUT_DIR/$VENDOR/${VENDOR}-cert.pem" "$SCP_TARGET/"
    scp "$CERT_OUT_DIR/$VENDOR/${VENDOR}-key.pem" "$SCP_TARGET/"
    # Chown to UID 65532 (distroless nonroot) so the container reads them.
    ssh "${SCP_TARGET%%:*}" "sudo chown -R 65532:65532 ${SCP_TARGET#*:}" || true
    echo "  cert tree placed at ${SCP_TARGET#*:} (uid 65532)"
  fi
fi

# -- Step 3: register / reconcile the four event topics ------------------
echo
echo "== fetching existing topics on portal =="
# Parallel arrays + a linear lookup instead of an associative array:
# operator-run on macOS /bin/bash 3.2 (#227). N is a handful of topics.
# The explicit counter stays : under
# `set -u`, bash < 4.4 aborts expanding an EMPTY array — the exact
# fresh-vendor case this script must handle. Indexed-array LENGTH is
# safe, but the counter keeps the intent loud.
EXISTING_TOPIC_KEYS=()
EXISTING_TOPIC_IDS=()
existing_topic_id_for() {
  # Search backwards: repeated keys keep the last row, like the old map.
  local i=${#EXISTING_TOPIC_KEYS[@]}
  while [ "$i" -gt 0 ]; do
    i=$((i - 1))
    if [ "${EXISTING_TOPIC_KEYS[$i]}" = "$1" ]; then
      printf '%s' "${EXISTING_TOPIC_IDS[$i]}"
      return 0
    fi
  done
  return 1
}
existing_topic_count=0
while IFS=$'\t' read -r id key; do
  [ -n "$key" ] || continue
  EXISTING_TOPIC_KEYS+=("$key")
  EXISTING_TOPIC_IDS+=("$id")
  existing_topic_count=$((existing_topic_count + 1))
done < <(curl -sk -H "Authorization: Bearer ${PORTAL_OPERATOR_API_KEY}" \
  "${PORTAL_URL}/api/vendors/${VENDOR}/topics" 2>/dev/null | jq -r '.topics[]? | "\(.id)\t\(.key_pattern)"' || true)
echo "  ${existing_topic_count} topic(s) currently registered for ${VENDOR}"

echo
echo "== upserting ${#TOPICS[@]} topics under '${VENDOR}' =="
created=0; patched=0; failed=0
for row in "${TOPICS[@]}"; do
  IFS='|' read -r key fmt schema_ref desc _msg_name <<<"${row}"
  existing_id="$(existing_topic_id_for "$key" || true)"
  if [ -n "$existing_id" ]; then
    patch_body=$(jq -nc \
      --arg fmt "$fmt" \
      --arg ref "$schema_ref" \
      --arg d "$desc" \
      '{format:$fmt, schema_ref:$ref, description:$d}')
    err_tmp=$(mktemp)
    if api PATCH "/api/vendors/${VENDOR}/topics/${existing_id}" "$patch_body" >/dev/null 2>"$err_tmp"; then
      echo "  ~ ${key} (id=${existing_id})"; patched=$((patched+1))
    else
      status=$(extract_status "$(<"$err_tmp")")
      echo "  ! ${key} PATCH failed (HTTP ${status:-?})"; failed=$((failed+1))
    fi
    rm -f "$err_tmp"
  else
    body=$(jq -nc \
      --arg key "$key" \
      --arg fmt "$fmt" \
      --arg ref "$schema_ref" \
      --arg d "$desc" \
      --argjson pts true \
      '{key_pattern:$key, format:$fmt, schema_ref:$ref, description:$d, publish_to_storage:$pts}')
    err_tmp=$(mktemp)
    if api POST "/api/vendors/${VENDOR}/topics" "$body" >/dev/null 2>"$err_tmp"; then
      echo "  + ${key}"; created=$((created+1))
    else
      status=$(extract_status "$(<"$err_tmp")")
      case "$status" in
        409) echo "  ~ ${key} already exists (409); leaving as-is"; patched=$((patched+1)) ;;
        *)   echo "  ! ${key} POST failed (HTTP ${status:-?})"; failed=$((failed+1)) ;;
      esac
    fi
    rm -f "$err_tmp"
  fi
done

# -- Step 4: build + upload .fds descriptors and flip format -------------
# Idempotent: build-fds.sh is a one-shot protoc invocation per .proto;
# upload-all.sh skips topics whose v1 schema body already matches by
# sha256 (MATCH) and PATCHes topic.format to `protobuf:<MsgName>` on
# --set-format. Re-running this script after the schemas land is safe.
#
# protoc is not bundled. If it isn't on PATH, we surface a clear
# instruction and SKIP the schema-publish stage; the topics stay
# format=json and the portal viewer / inspector decode paths simply
# don't light up until protoc is installed and this script is re-run.
SCHEMA_DIR="$(dirname "$0")/airdefenders-schemas"
echo
echo "== building + uploading protobuf descriptors (.fds) =="
if ! command -v protoc >/dev/null 2>&1; then
  echo "  SKIP: protoc not on PATH. Install protobuf-compiler (apt) or"
  echo "        brew install protobuf (macOS), then re-run this script."
  echo "        Topics remain format=json until then; nothing breaks."
  schema_failed=0
else
  if [ ! -d "$SCHEMA_DIR" ]; then
    echo "  SKIP: $SCHEMA_DIR not found. The schemas dir is part of this"
    echo "        repo; if missing, the proto sources weren't checked out."
    schema_failed=0
  else
    pushd "$SCHEMA_DIR" >/dev/null
    if ! bash ./build-fds.sh; then
      echo "  ERROR: build-fds.sh failed; not attempting upload."
      schema_failed=1
    else
      # upload-all.sh uses PORTAL_OPERATOR_API_KEY (or PORTAL_API_KEY)
      # + PORTAL_URL + SITE_ID — already exported in this script's env.
      if SITE_ID="$SITE_ID" PORTAL_URL="$PORTAL_URL" \
         PORTAL_OPERATOR_API_KEY="$PORTAL_OPERATOR_API_KEY" \
         bash ./upload-all.sh --set-format; then
        echo "  schemas uploaded + topic.format PATCH'd to protobuf:<MsgName>"
        schema_failed=0
      else
        echo "  ERROR: upload-all.sh failed; topics may be mid-state"
        echo "        (some uploaded, some not). Re-run; the script is"
        echo "        idempotent — skipping topics that already match."
        schema_failed=1
      fi
    fi
    popd >/dev/null
  fi
fi

echo
echo "== summary =="
echo "  vendor:       ${VENDOR}"
echo "  topics:       created=${created} patched=${patched} failed=${failed}"
echo "  schemas:      $([ "${schema_failed:-0}" -eq 0 ] && echo "ok (format=protobuf:<MsgName>)" || echo "FAILED — see log above")"
if [ -n "$VENDOR_API_KEY" ]; then
  echo "  api_key:      ${VENDOR_API_KEY}    (one-time; capture in 1Password)"
fi
echo
echo "Next: run ansible-playbook -i inventory/hosts.ini -i inventory/mgmt-hosts-mesh.ini ops/ansible/airdefenders-deploy.yml"
exit $((failed + ${schema_failed:-0}))
