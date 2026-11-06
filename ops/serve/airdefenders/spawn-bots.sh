#!/usr/bin/env bash
# spawn-bots.sh — open one browser window per Air Defenders auto-pilot persona
# so you can watch the 8 bots play co-op multiplayer and the resulting data flow
# over the CLI (goat sub --tui), panoscope, and flashlight. macOS + Chrome.
#
#   ./spawn-bots.sh                                  # 8 bots vs http://localhost:8095/mp
#   ./spawn-bots.sh http://airdefenders.netbird.legacy.example   # custom host/base
#   AD_BOTS="trigger deadeye nomad" ./spawn-bots.sh  # spawn a subset
#   ./spawn-bots.sh --kill                           # close the bot windows + temp profiles
#
# Each window uses a throwaway Chrome profile (--user-data-dir under $TMPDIR) so
# it is a fully independent player (separate WebSocket + sessionStorage) and
# never touches your real Chrome profile. Eight bots fill the room (maxPlayers=8);
# observe externally rather than joining a 9th player.
#
# The server must allow >= N WebSocket connections from your IP. The default cap
# is already 8 (defaultMaxConnsPerIP); raise it if you also want to join:
#   AD_MAX_CONNS_PER_IP=9 ./airdefenders
set -euo pipefail

BROWSER="${BROWSER:-Google Chrome}"
PROFILE_ROOT="${TMPDIR:-/tmp}/ad-bots"
# Order matches the personae table in docs/design/airdefenders-multiplayer.md.
ALL_BOTS=(trigger deadeye gremlin dozer sloppy squint nomad camel)

if [[ "${1:-}" == "--kill" ]]; then
  pkill -f "user-data-dir=${PROFILE_ROOT}" 2>/dev/null || true
  sleep 1
  rm -rf "${PROFILE_ROOT}"
  echo "closed bot windows + removed ${PROFILE_ROOT}"
  exit 0
fi

BASE="${1:-${AD_URL:-http://localhost:8095}}"
BASE="${BASE%/}"
case "$BASE" in
  */mp) MP_URL="$BASE" ;;
  *)    MP_URL="$BASE/mp" ;;
esac

read -r -a BOTS <<< "${AD_BOTS:-${ALL_BOTS[*]}}"
N=${#BOTS[@]}

# Main-display size → grid layout (fallback 1920x1080 if AppleScript is unavailable).
bounds=$(osascript -e 'tell application "Finder" to get bounds of window of desktop' 2>/dev/null || echo "0, 0, 1920, 1080")
SCREEN_W=$(echo "$bounds" | awk -F', ' '{print $3}'); SCREEN_W="${SCREEN_W:-1920}"
SCREEN_H=$(echo "$bounds" | awk -F', ' '{print $4}'); SCREEN_H="${SCREEN_H:-1080}"

COLS="${COLS:-4}"; (( COLS > N )) && COLS=$N
ROWS=$(( (N + COLS - 1) / COLS ))
W=$(( SCREEN_W / COLS ))
H=$(( SCREEN_H / ROWS ))

echo "spawning ${N} bot(s) → ${MP_URL}   grid ${COLS}x${ROWS} @ ${W}x${H}   browser='${BROWSER}'"
i=0
for bot in "${BOTS[@]}"; do
  col=$(( i % COLS )); row=$(( i / COLS ))
  x=$(( col * W )); y=$(( row * H ))
  prof="${PROFILE_ROOT}/${bot}"
  mkdir -p "$prof"
  if open -na "$BROWSER" --args \
      --user-data-dir="$prof" \
      --no-first-run --no-default-browser-check \
      --new-window \
      --window-position="${x},${y}" \
      --window-size="${W},${H}" \
      "${MP_URL}?bot=${bot}" >/dev/null 2>&1; then
    echo "  ${bot}  @ ${x},${y}"
  else
    echo "  ! failed to launch ${bot} (is '${BROWSER}' installed?)"
  fi
  i=$(( i + 1 ))
  sleep 0.4   # stagger so the Chrome instances don't race the profile lock
done
echo "done. close them with: $0 --kill"
