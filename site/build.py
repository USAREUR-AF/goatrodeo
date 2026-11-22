#!/usr/bin/env python3
"""Build and check the landing page in site/.

  python3 site/build.py          # copy the game and the captures in, check the page

Copies two things the page uses but does not keep a second copy of:
  ops/serve/airdefenders/index.html  ->  site/play/index.html   (the real solo game)
  docs/media/*.png, *.webp           ->  site/assets/media/       (the real captures)

Then fails if a number the page states differs from the tree, or if the code
shown faintly behind the first screen is no longer in multiplayer.go.
"""
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SITE = ROOT / "site"
GAME = ROOT / "ops/serve/airdefenders"


def copy():
    (SITE / "play").mkdir(exist_ok=True)
    shutil.copyfile(GAME / "index.html", SITE / "play/index.html")
    media = SITE / "assets/media"
    media.mkdir(parents=True, exist_ok=True)
    for f in sorted((ROOT / "docs/media").iterdir()):
        if f.suffix in (".png", ".webp"):
            shutil.copyfile(f, media / f.name)


def measured():
    mp = (GAME / "multiplayer.go").read_text()
    manifest = (ROOT / "ops/zenoh-trial-peers/airdefenders-schemas/topic-manifest.tsv").read_text()
    mp_html = (GAME / "mp.html").read_text()
    return {
        # grep -E '^\s*tickHz\s*=' ops/serve/airdefenders/multiplayer.go
        "tick-hz": re.search(r"^\s*tickHz\s*=\s*(\d+)", mp, re.M).group(1),
        # grep -E '^\s*maxPlayers\s*=' ops/serve/airdefenders/multiplayer.go
        "max-players": re.search(r"^\s*maxPlayers\s*=\s*(\d+)", mp, re.M).group(1),
        # grep -c '^airdefenders/' ops/zenoh-trial-peers/airdefenders-schemas/topic-manifest.tsv
        "event-keys": str(sum(1 for l in manifest.splitlines() if l.startswith("airdefenders/"))),
        # wc -c ops/serve/airdefenders/index.html, in KiB
        "game-kib": str(round((GAME / "index.html").stat().st_size / 1024)),
        # grep -cE "^\s+\w+:\s*\{\s*label:" ops/serve/airdefenders/mp.html  (the auto-pilot personas)
        "bots": str(len(re.findall(r"^\s+\w+:\s*\{\s*label:", mp_html, re.M))),
    }


def check():
    page = (SITE / "index.html").read_text()
    errors = []
    want = measured()
    for key, value in want.items():
        found = re.findall(rf'data-n="{key}">([^<]*)<', page)
        if not found:
            errors.append(f"{key}: the page does not state it")
        for f in found:
            if f.strip() != value:
                errors.append(f"{key}: the page says {f.strip()}, the tree says {value}")
    code = re.search(r'<pre class="bgcode"[^>]*>(.*?)</pre>', page, re.S).group(1)
    code = code.replace("&lt;", "<").replace("&gt;", ">").replace("&amp;", "&")
    if code.strip("\n") not in (GAME / "multiplayer.go").read_text():
        errors.append("the code behind the first screen is no longer in ops/serve/airdefenders/multiplayer.go")
    for e in errors:
        print("site check:", e, file=sys.stderr)
    if not errors:
        print("site check: " + ", ".join(f"{k}={v}" for k, v in want.items()))
    return not errors


if __name__ == "__main__":
    copy()
    ok = check()
    # no command box cut off at 1440 or 390 px, no output box at 1440 (headless Chrome)
    fit = subprocess.run([sys.executable, str(SITE / "fitcheck.py"), str(SITE), "index.html"]).returncode == 0
    sys.exit(0 if ok and fit else 1)
