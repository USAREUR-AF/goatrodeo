#!/usr/bin/env python3
"""Check that no command box on a page is cut off.

Loads each page in headless Chrome at 1440 and 390 px wide and fails when:
  - a command box (an element with data-box="command", or inside one) is wider
    than its box at either width: a command must show whole, wrapping at spaces;
  - an output box (data-box="output") is wider than its box at 1440 px
    (at 390 px it may scroll inside its box, and its caption says so);
  - a <pre> carries neither mark, so a new box cannot skip the check
    (decoration inside aria-hidden="true" is exempt);
  - the page itself scrolls sideways.

Usage: fitcheck.py <root-dir> <page> [<page> ...]
  <root-dir> is served over HTTP on 127.0.0.1; each <page> is a path under it,
  for example "index.html". Chrome: $CHROME, else google-chrome / chromium on
  PATH, else the macOS application. With no Chrome the check fails under CI
  (the CI variable is set) and is skipped with a notice otherwise.
"""
import functools
import http.server
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading

WIDTHS = (1440, 390)

HARNESS = """<!doctype html><meta charset="utf-8"><title>fitcheck</title>
<body style="margin:0"><pre id="fit-result">pending</pre><script>
const page = %(page)s, widths = %(widths)s, out = [];
function measure(doc, w) {
  const bad = [];
  const label = el => el.textContent.trim().replace(/\\s+/g, ' ').slice(0, 80);
  const root = doc.documentElement;
  if (root.scrollWidth > root.clientWidth + 1)
    bad.push('the page scrolls sideways at ' + w + ' px (' + root.scrollWidth + ' > ' + root.clientWidth + ')');
  const seen = new Set();
  for (const el of doc.querySelectorAll('pre, [data-box]')) {
    if (el.closest('[aria-hidden="true"]')) continue;
    const box = el.closest('[data-box]');
    if (!box) { bad.push('a <pre> with no data-box mark: "' + label(el) + '"'); continue; }
    if (seen.has(box)) continue;
    seen.add(box);
    const kind = box.dataset.box;
    if (kind !== 'command' && kind !== 'output') { bad.push('data-box="' + kind + '" is not command or output'); continue; }
    if (kind === 'output' && w < 1440) continue;
    for (const e of [box, ...box.querySelectorAll('pre')]) {
      if (e.offsetParent === null) continue;
      if (e.scrollWidth > e.clientWidth + 1) {
        bad.push(kind + ' box cut off at ' + w + ' px (' + e.scrollWidth + ' > ' + e.clientWidth + '): "' + label(box) + '"');
        break;
      }
    }
  }
  return bad;
}
function run(i) {
  if (i >= widths.length) { document.getElementById('fit-result').textContent = JSON.stringify(out); return; }
  const w = widths[i], f = document.createElement('iframe');
  f.style.cssText = 'border:0;display:block;width:' + w + 'px;height:900px';
  f.onload = () => setTimeout(() => {
    try { out.push({ width: w, bad: measure(f.contentDocument, w) }); }
    catch (e) { out.push({ width: w, bad: ['could not read the page: ' + e] }); }
    f.remove(); run(i + 1);
  }, 1500);
  f.src = page;
  document.body.appendChild(f);
}
run(0);
</script>"""


def find_chrome():
    if os.environ.get("CHROME"):
        return os.environ["CHROME"]
    for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"):
        p = shutil.which(name)
        if p:
            return p
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    return mac if os.path.exists(mac) else None


class Handler(http.server.SimpleHTTPRequestHandler):
    harness = {}

    def do_GET(self):
        body = self.harness.get(self.path.split("?")[0])
        if body is None:
            return super().do_GET()
        data = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass


def check(root, pages):
    chrome = find_chrome()
    if not chrome:
        if os.environ.get("CI"):
            print("fitcheck: no Chrome found (set CHROME)", file=sys.stderr)
            return 1
        print("fitcheck: skipped, no Chrome found (set CHROME to run it)")
        return 0
    handler = functools.partial(Handler, directory=root)
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    port = srv.server_address[1]
    failures = 0
    try:
        for n, page in enumerate(pages):
            url = "/" + page.lstrip("/")
            Handler.harness["/__fitcheck_%d__.html" % n] = HARNESS % {
                "page": json.dumps(url), "widths": json.dumps(list(WIDTHS))}
            with tempfile.TemporaryDirectory() as prof:
                cmd = [chrome, "--headless", "--disable-gpu", "--no-sandbox", "--no-first-run",
                       "--hide-scrollbars", "--user-data-dir=" + prof, "--window-size=1500,1000",
                       "--virtual-time-budget=15000", "--dump-dom",
                       "http://127.0.0.1:%d/__fitcheck_%d__.html" % (port, n)]
                dom = subprocess.run(cmd, capture_output=True, text=True, timeout=180).stdout
            m = re.search(r'<pre id="fit-result">(.*?)</pre>', dom, re.S)
            raw = m.group(1) if m else ""
            raw = raw.replace("&lt;", "<").replace("&gt;", ">").replace("&quot;", '"').replace("&amp;", "&")
            try:
                results = json.loads(raw)
            except ValueError:
                print("fitcheck: %s: Chrome returned no result" % page, file=sys.stderr)
                failures += 1
                continue
            for r in results:
                for b in r["bad"]:
                    print("fitcheck: %s @%d: %s" % (page, r["width"], b), file=sys.stderr)
                    failures += 1
            if all(not r["bad"] for r in results):
                print("fitcheck: %s: every box fits at %s px" % (page, " and ".join(map(str, WIDTHS))))
    finally:
        srv.shutdown()
    return 1 if failures else 0


if __name__ == "__main__":
    if len(sys.argv) < 3:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    sys.exit(check(os.path.abspath(sys.argv[1]), sys.argv[2:]))
