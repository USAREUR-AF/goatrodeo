#!/usr/bin/env node
// Refresh the captures in this directory from a locally running Air Defenders.
// How to run it, and what each capture shows: CAPTURES.md, beside this file.
//
//   node docs/media/capture.js stills|coop [base-url] [output-dir]
//
// Needs the `playwright` package where Node can find it (NODE_PATH works), a
// Chromium it can launch, and `ffmpeg` and `img2webp` (from libwebp) on PATH.
// The animation's frames are scaled down for size; nothing is drawn on,
// cropped or retouched.
'use strict';
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');

const MODE = process.argv[2];
const BASE = (process.argv[3] || 'http://localhost:8095').replace(/\/$/, '');
const OUT = path.resolve(process.argv[4] || __dirname);
const GAME = { width: 800, height: 700 };      // the game's own canvas resolution
const WIDE = { width: 1440, height: 810 };     // the eight-frame demo page
const COOP_RECORD_S = 40;                      // seconds of co-op play to record
// Which stretch of the recording becomes the animation. Look at the recording
// first; COOP_FROM=<file.webm> cuts again without recording again.
const COOP_START_S = Number(process.env.COOP_START_S || 20);
const COOP_LENGTH_S = Number(process.env.COOP_LENGTH_S || 8);

async function soloStills(browser) {
  const page = await browser.newPage({ viewport: GAME });
  await page.goto(BASE + '/');
  await page.waitForTimeout(1500);
  await page.screenshot({ path: path.join(OUT, 'airdefenders-title-screen.png') });

  await page.keyboard.press('Space');            // title -> briefing
  await page.waitForTimeout(1500);
  await page.keyboard.press('Space');            // briefing -> combat
  await page.waitForTimeout(1000);
  await page.keyboard.press('Tab');              // unarmed -> armed
  await page.keyboard.press('Tab');              // armed -> autopilot
  for (let i = 0; i < 12; i++) {                 // launch, nudge the launcher, repeat
    await page.keyboard.press('Space');
    await page.keyboard.down(i % 2 ? 'KeyA' : 'KeyD');
    await page.waitForTimeout(350);
    await page.keyboard.up(i % 2 ? 'KeyA' : 'KeyD');
    await page.waitForTimeout(400);
  }
  await page.screenshot({ path: path.join(OUT, 'airdefenders-solo-play.png') });
  await page.close();
}

async function coopRecording(browser) {
  // Five auto-pilot personas share one room. The first one is filmed with
  // Playwright's own page recorder; the other four play in a second context.
  // Record on a freshly started server: a player who joins a wave already
  // under way waits for the next one.
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'airdefenders-video-'));
  const filmed = await browser.newContext({ viewport: GAME, recordVideo: { dir, size: GAME } });
  const others = await browser.newContext({ viewport: GAME });
  const page = await filmed.newPage();
  await page.goto(BASE + '/mp?bot=deadeye');
  for (const bot of ['trigger', 'nomad', 'camel', 'squint']) {
    const p = await others.newPage();
    await p.goto(BASE + '/mp?bot=' + bot);
  }
  await page.bringToFront();
  await page.waitForTimeout(COOP_RECORD_S * 1000);
  await others.close();
  await filmed.close();                          // closing the context writes the video
  return path.join(dir, fs.readdirSync(dir).find(f => f.endsWith('.webm')));
}

function coopAnimation(webm) {
  // Cut one stretch out of the recording at 12 frames a second, 640 pixels
  // wide. The only filters are the frame rate and the scale.
  const frames = fs.mkdtempSync(path.join(os.tmpdir(), 'airdefenders-frames-'));
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-ss', String(COOP_START_S), '-t', String(COOP_LENGTH_S),
    '-i', webm, '-vf', 'fps=12,scale=640:-1:flags=lanczos', path.join(frames, '%03d.png')], { stdio: 'inherit' });
  const files = fs.readdirSync(frames).sort().map(f => path.join(frames, f));
  execFileSync('img2webp', ['-loop', '0', '-lossy', '-q', '55', '-m', '4', '-d', '83', ...files,
    '-o', path.join(OUT, 'airdefenders-coop-bots.webp')], { stdio: 'inherit' });
  fs.rmSync(frames, { recursive: true, force: true });
}

async function fabricDemoStill(browser) {
  const page = await browser.newPage({ viewport: WIDE });
  await page.goto(BASE + '/demo');
  await page.waitForTimeout(30000);              // eight bots, well into wave 1
  await page.screenshot({ path: path.join(OUT, 'airdefenders-fabric-demo-eight-bots.png') });
  await page.close();
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  if (process.env.COOP_FROM) return coopAnimation(process.env.COOP_FROM);
  if (MODE !== 'stills' && MODE !== 'coop') throw new Error('usage: capture.js stills|coop [base-url] [output-dir]');
  const { chromium } = require('playwright');   // not needed to cut an existing recording
  const browser = await chromium.launch();
  let webm;
  try {
    if (MODE === 'stills') {
      await soloStills(browser);
      await fabricDemoStill(browser);
    } else {
      webm = await coopRecording(browser);
    }
  } finally {
    await browser.close();
  }
  if (webm) {
    console.log('co-op recording kept at ' + webm);
    coopAnimation(webm);
  }
})().catch(err => { console.error(err); process.exit(1); });
