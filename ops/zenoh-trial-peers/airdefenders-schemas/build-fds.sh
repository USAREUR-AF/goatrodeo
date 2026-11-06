#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# build-fds.sh — compile every .proto under proto-airdefenders/ into a
# per-topic FileDescriptorSet (.fds) inside out/. The portal accepts a
# .fds as its schema body — that's the protoc output that includes the
# full descriptor tree, suitable for dynamicpb-based decode and for the
# /api/schemas/render endpoint that walks the descriptor.
#
# Mirrors the synth-vendor-schemas/build-fds.sh shape verbatim so
# operators familiar with one understand the other.
#
# Requires `protoc` on PATH (apt: protobuf-compiler / brew: protobuf).
#
# Usage:
#   ./build-fds.sh                                    # compile all four topics
#   ./build-fds.sh proto-airdefenders/foo.proto       # compile a single file
#
# Output: out/<basename>.fds for each compiled .proto. common.proto is
# imported by every per-event proto; --include_imports embeds its
# descriptor in each .fds so the portal can resolve Vec2 without a
# separate upload.

set -euo pipefail

cd "$(dirname "$0")"

if ! command -v protoc >/dev/null 2>&1; then
  echo "ERROR: protoc not found on PATH. Install protobuf-compiler / brew install protobuf." >&2
  exit 1
fi

mkdir -p out

# When called with explicit args, build those files; otherwise iterate
# the topic manifest.
if [ "$#" -gt 0 ]; then
  for src in "$@"; do
    base=$(basename "${src%.proto}")
    out="out/${base}.fds"
    protoc --descriptor_set_out="${out}" --include_imports --include_source_info \
      --proto_path=proto-airdefenders "${src#proto-airdefenders/}"
    echo "built ${out} ($(wc -c < "${out}" | tr -d ' ') bytes)"
  done
  exit 0
fi

# Manifest path: topic<TAB>proto<TAB>msg<TAB>wire_format
# We only need proto_file in this loop; the other columns are consumed
# by upload-all.sh.
while IFS=$'\t' read -r _topic_key proto_file _msg _fmt; do
  case "$_topic_key" in '#'*|'') continue ;; esac
  base=$(basename "${proto_file%.proto}")
  out="out/${base}.fds"
  protoc --descriptor_set_out="${out}" --include_imports --include_source_info \
    --proto_path=proto-airdefenders "$(basename "${proto_file}")"
  echo "built ${out} for ${_topic_key} ($(wc -c < "${out}" | tr -d ' ') bytes)"
done < topic-manifest.tsv
