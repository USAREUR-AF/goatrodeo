# The landing page

A single static page, published to GitHub Pages by `.github/workflows/pages.yml`.
No framework, no build step beyond copying, no external fonts, scripts or requests.

```bash
python3 site/build.py                                  # copy in, then check
python3 -m http.server 8000 --directory site           # look at it on http://localhost:8000/
```

## What each file is

| File | What it is |
|---|---|
| `index.html` | the page: markup, CSS and the few lines of script (copy button, "Play it here") |
| `assets/icon.svg` | the tab icon |
| `build.py` | copies in what the page shows but does not keep twice, then checks the page against the tree |
| `.gitignore` | keeps the copied files out of git |
| `play/index.html` | copied by `build.py` from `ops/serve/airdefenders/index.html`: the real solo game |
| `assets/media/` | copied by `build.py` from `docs/media/`: the real captures; how each was made is in `docs/media/CAPTURES.md` |

"Play it here" loads the solo game from `play/` in place of the capture. With no
server behind it, its calls to `/api/me` and `/api/scores` fail and the score
board uses the browser's own copy, as the game already does when the network is away.

## Where each number comes from

`build.py` re-measures each one from the tree and fails the build when the page says
something else. The page marks each with `data-n`.

| Page says | `data-n` | Measured by |
|---|---|---|
| 30 simulation steps a second | `tick-hz` | `grep -E '^\s*tickHz\s*=' ops/serve/airdefenders/multiplayer.go` |
| 8 players in one co-op room | `max-players` | `grep -E '^\s*maxPlayers\s*=' ops/serve/airdefenders/multiplayer.go` |
| 4 event keys published | `event-keys` | `grep -c '^airdefenders/' ops/zenoh-trial-peers/airdefenders-schemas/topic-manifest.tsv` |
| 104 KiB, the solo game | `game-kib` | `wc -c ops/serve/airdefenders/index.html`, divided by 1024 and rounded |
| 8 bots | `bots` | `grep -cE '^\s+\w+:\s*\{\s*label:' ops/serve/airdefenders/mp.html` (the auto-pilot personas) |

The code shown faintly behind the first screen is the room's tick loop; `build.py`
fails if that text is no longer in `ops/serve/airdefenders/multiplayer.go`.
