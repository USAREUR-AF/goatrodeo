#!/usr/bin/env bash
# .github/scripts/base-images-shared.sh — this repository's copies of goatcore's base-image files equal goatcore main's.
#
#   bash .github/scripts/base-images-shared.sh          # check: fails on any difference
#   bash .github/scripts/base-images-shared.sh --fix    # copy goatcore main's bytes, then sync the Dockerfile defaults
#
# goatcore owns ops/base-images/base-images.py and every role file it defines
# (goatcore ADR 1220, docs/design/base-images.md §Refresh): its Dependabot refreshes
# them. This repository carries copies of the ones it uses, and an environment's
# assembly refuses a shared path whose bytes differ. A role file only this repository
# defines (not present in goatcore) is its own and is not compared.
# GOATCORE_URL and GOATCORE_REF override the source (default: USAREUR-AF/goatcore main).
set -euo pipefail
FIX=0
[ "${1:-}" = "--fix" ] && FIX=1
URL="${GOATCORE_URL:-https://github.com/USAREUR-AF/goatcore}"
REF="${GOATCORE_REF:-main}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
git -C "$tmp" remote add origin "$URL"
git -C "$tmp" fetch -q --depth 1 --filter=blob:none origin "$REF"
core="$(git -C "$tmp" rev-parse FETCH_HEAD)"
echo "base-images-shared: comparing with goatcore ${core:0:12} ($REF)"

differ=0
while IFS= read -r -d '' f; do
  want="$(git -C "$tmp" ls-tree "$core" -- "$f" | awk '{print $3}')"
  [ -n "$want" ] || continue  # this repository's own role
  have="$(git hash-object -- "$f")"
  [ "$have" = "$want" ] && continue
  differ=$((differ + 1))
  if [ "$FIX" = 1 ]; then
    git -C "$tmp" cat-file blob "$want" > "$f"
    echo "copied  $f from goatcore ${core:0:12}"
  else
    echo "::error file=$f::$f differs from goatcore ${core:0:12}. Run: bash .github/scripts/base-images-shared.sh --fix"
  fi
done < <(find ops/base-images -type f -print0 | sort -z)

if [ "$FIX" = 1 ]; then
  [ "$differ" = 0 ] || python3 ops/base-images/base-images.py sync
  echo "base-images-shared: $differ file(s) copied"
  exit 0
fi
[ "$differ" = 0 ] || { echo "base-images-shared: $differ file(s) differ from goatcore"; exit 1; }
echo "base-images-shared: every shared file equals goatcore's"
