#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# upload-all.sh — reconcile every airdefenders topic against the live
# portal: upload the matching .fds as the v1 schema body, and (with
# --set-format) PATCH the topic format to `protobuf:<MsgName>` so the
# portal's schema viewer + per-language autogen-code surfaces light up.
#
# Idempotent:
#   - If the topic already has a v1 schema with matching sha256 → no-op.
#   - If the topic already has a v1 schema with different content →
#     emits SKIP (operator must delete the remote or bump to v2).
#   - If no v1 exists → POSTs the .fds.
#
# Mirrors ops/zenoh-trial-peers/synth-vendor-schemas/upload-all.sh; the
# only differences are the proto-dir, the manifest path, and the
# SITE_ID-aware topic-key lookup (the airdefenders manifest carries a
# literal `<SITE>` placeholder so build-fds.sh can run site-agnostic).
#
# Requires:
#   - PORTAL_URL — e.g. https://portal.nova.netbird.nova.example
#   - PORTAL_API_KEY OR PORTAL_OPERATOR_API_KEY — operator-scope key
#     (vendor-scoped keys cannot PATCH topic.format)
#   - jq, curl, shasum (or sha256sum on linux)
#
# Usage:
#   PORTAL_URL=https://portal.nova.netbird.nova.example \
#   PORTAL_OPERATOR_API_KEY=$NOVA_PORTAL_OPERATOR_KEY \
#     ./upload-all.sh                       # upload .fds only
#
#   ./upload-all.sh --set-format            # also PATCH topic.format
#                                           # to protobuf:<MsgName>
#
#   SITE_ID=nethost ./upload-all.sh            # override nova-default site

set -euo pipefail

cd "$(dirname "$0")"

: "${PORTAL_URL:?PORTAL_URL not set}"
PORTAL_URL="${PORTAL_URL%/}"
# Accept either env-var name for the API key. Operator-key required.
API_KEY="${PORTAL_API_KEY:-${PORTAL_OPERATOR_API_KEY:-}}"
: "${API_KEY:?set PORTAL_API_KEY or PORTAL_OPERATOR_API_KEY}"

SITE_ID="${SITE_ID:-nova}"
VENDOR="airdefenders"

SET_FORMAT=0
for arg in "$@"; do
  case "$arg" in
    --set-format) SET_FORMAT=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

if [ ! -d out ]; then
  echo "out/ does not exist — run ./build-fds.sh first." >&2
  exit 1
fi

curl_auth() {
  curl -sS -k -H "Authorization: Bearer ${API_KEY}" "$@"
}

# Portable sha256 (macOS = shasum -a 256; linux = sha256sum)
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Resolve the manifest's literal `<SITE>` placeholder against $SITE_ID
# at lookup time. Each topic in the portal carries the rendered key
# (e.g. airdefenders/nova/radar/detection/v1).
expand_site() {
  printf '%s\n' "$1" | sed "s|<SITE>|${SITE_ID}|g"
}

# Fetch topic id for (vendor, key_pattern). Echos id or empty string.
lookup_topic_id() {
  local vendor="$1" key="$2"
  curl_auth "${PORTAL_URL}/api/vendors/${vendor}/topics" |
    jq -r --arg k "$key" '.topics[]? | select(.key_pattern==$k) | .id' |
    head -1
}

upload_one() {
  local key_template="$1" proto_file="$2" msg_name="$3"
  local key
  key=$(expand_site "$key_template")
  local base fds
  base=$(basename "${proto_file%.proto}")
  fds="out/${base}.fds"
  if [ ! -f "$fds" ]; then
    echo "MISS  ${key}: ${fds} not built (run ./build-fds.sh)" >&2
    return 1
  fi
  local topic_id
  topic_id=$(lookup_topic_id "$VENDOR" "$key" || true)
  if [ -z "$topic_id" ]; then
    echo "MISS  ${key}: topic not registered; run bootstrap-airdefenders-portal.sh first"
    return 1
  fi

  # Probe existing v1 schema for this topic.
  local existing
  existing=$(curl_auth "${PORTAL_URL}/api/vendors/${VENDOR}/topics/${topic_id}/schemas" |
    jq -r '.schemas[]? | select(.version=="v1") | .sha256' | head -1)
  local local_sha
  local_sha=$(sha256_of "$fds")
  if [ -n "$existing" ]; then
    if [ "$existing" = "$local_sha" ]; then
      echo "MATCH ${key}: v1 already uploaded with matching sha256"
    else
      echo "SKIP  ${key}: v1 exists with different content (remote=${existing:0:12}.. local=${local_sha:0:12}..); delete remote or bump to v2"
    fi
  else
    echo "UPLD  ${key}: posting v1 ($(wc -c < "$fds" | tr -d ' ') bytes)"
    local resp
    resp=$(curl_auth -X POST \
      -F "file=@${fds}" \
      -F "version=v1" \
      -F "compatibility=none" \
      -F "format=protobuf-fds" \
      "${PORTAL_URL}/api/vendors/${VENDOR}/topics/${topic_id}/schemas")
    local err
    err=$(echo "$resp" | jq -r '.error.message // empty')
    if [ -n "$err" ]; then
      echo "ERROR ${key}: $err"
      return 1
    fi
  fi

  if [ "$SET_FORMAT" = "1" ]; then
    echo "FMTSET ${key}: PATCH format → protobuf:${msg_name}"
    curl_auth -X PATCH \
      -H "Content-Type: application/json" \
      -d "{\"format\":\"protobuf:${msg_name}\"}" \
      "${PORTAL_URL}/api/vendors/${VENDOR}/topics/${topic_id}" >/dev/null
  fi
}

ok=0
fail=0
while IFS=$'\t' read -r key_template proto_file msg_name _fmt; do
  case "$key_template" in '#'*|'') continue ;; esac
  if upload_one "$key_template" "$proto_file" "$msg_name"; then
    ok=$((ok+1))
  else
    fail=$((fail+1))
  fi
done < topic-manifest.tsv

echo
echo "summary: ${ok} ok, ${fail} failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
