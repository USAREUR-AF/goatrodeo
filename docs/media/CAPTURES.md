# How the captures were made

Every image in this directory is a capture of the real thing running on a laptop. Nothing is mocked up, drawn on, cropped or retouched; the only processing is scaling the animation down and compressing it. If a capture goes stale, make it again the same way and replace the file under the same name.

| File | Shows | Viewport |
|---|---|---|
| `airdefenders-coop-bots.webp` | co-op room, five bots, seen from the `deadeye` bot's page; 8 seconds at 12 frames a second | 800 × 700, scaled to 640 wide |
| `airdefenders-title-screen.png` | solo title screen | 800 × 700 |
| `airdefenders-solo-play.png` | solo play, wave 1, driven by key presses | 800 × 700 |
| `airdefenders-fabric-demo-eight-bots.png` | the `/demo` page with all eight bots, 30 seconds after it opened | 1440 × 810 |

## What you need

- Go, the version in `ops/serve/airdefenders/go.mod`
- Node.js with the `playwright` package and a Chromium it can launch
- `ffmpeg`, and `img2webp` from libwebp

## 1. Start the game

```bash
cd ops/serve/airdefenders
DB_PATH=./local-scores.db AD_DEMO_UNGATED=1 SITE_ID=zagros go run .
```

`AD_DEMO_UNGATED=1` lets the `/demo` page open without a sign-in. `SITE_ID=zagros` puts the example goatnet's name in the demo page's header; never capture against a real goatnet, whose names must not appear in this repository.

## 2. Take the stills

From the repository root, in a second terminal:

```bash
node docs/media/capture.js stills
```

If Node cannot find `playwright`, point `NODE_PATH` at the `node_modules` directory that holds it.

[`capture.js`](capture.js) drives a headless Chromium and does this:

1. **Title screen.** Opens `/`, waits 1.5 seconds, captures.
2. **Solo play.** Presses Space twice (start, then ready), Tab twice (fire mode to autopilot), then twelve rounds of Space with A or D held for a moment, and captures. What is on screen at that moment depends on how the wave went, so the picture differs from run to run; update its alt text in the README to match.
3. **Fabric demo.** Opens `/demo`, waits 30 seconds for the eight bots to get into combat, captures.

## 3. Record the animation

Stop the game, start it again (step 1) so the co-op room is new, then:

```bash
node docs/media/capture.js coop
```

This opens `/mp?bot=deadeye` in a page that Playwright records as video, opens `/mp?bot=trigger`, `nomad`, `camel` and `squint` in four more pages, and lets them play for 40 seconds. It prints where it kept the recording, then cuts 8 seconds out of it, starting 20 seconds in, into `airdefenders-coop-bots.webp`.

Watch the recording before you accept the cut. To choose a different stretch without recording again:

```bash
COOP_FROM=/path/to/recording.webm COOP_START_S=24 COOP_LENGTH_S=8 node docs/media/capture.js
```

The animation in this directory is that cut: the 8 seconds from 24 seconds in, which is the middle of wave 2.

The room has to be new because a player who joins a wave already under way waits for the next one, which makes for a dull film.

## 4. Check the sizes

Keep each file under about 400 KB and the directory under about 2 MB, so the README stays quick to load:

```bash
ls -l docs/media
```

## Adding a capture for a new game

Run the real thing, capture it with a script someone else can run, give the file a descriptive lowercase-hyphenated name, add a row and a section here, and give the image alt text in the README that says what is in the picture. If something cannot run on a laptop, say so in the README and leave the picture out.
