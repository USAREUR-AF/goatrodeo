#!/usr/bin/env python3
"""The leak gate for pull requests: scan the lines a change adds, never print what matched.

    scan-diff.py --base <sha> --head <sha> [--repo <dir>] [--needles-file <file>]
    scan-diff.py --self-test

Reads the lines added between the merge base of <base> and <head> (git diff,
no context) and fails on two classes:

  derived-token  a name of the maintainers' own deployments. The list is not
                 in this repository: it arrives in the environment variable
                 GOAT_LEAK_NEEDLES (an encrypted repository secret: base64 of
                 the newline-separated list, gzip-compressed or plain), or in
                 --needles-file. Matching is case-insensitive substring.
  shapes         material that is never right in this repository whatever its
                 value: a PEM private-key block, AWS access key ids, JWTs,
                 agent-session URLs, releasability markings.

A line carrying `oss-allow: <reason>` is exempt from both classes, as in the
export's own gate; each exemption is listed as a notice for the reviewer.

A hit prints as <file>:<line>: <class> and, for a derived token, the first 12
hex characters of the sha256 of the lower-cased needle. Neither the needle nor
the line is ever printed. Under GitHub Actions each hit is also an annotation.

Exit: 0 clean, 1 hits, 2 usage error or no needle list (fails closed).
"""
import argparse
import base64
import binascii
import gzip
import hashlib
import os
import re
import subprocess
import sys
import tempfile

SHAPES = [
    # the header standing alone on its line is a key block; inside a string literal it is code
    ("pem-private", r"^\s*-----BEGIN (RSA |EC |OPENSSH |DSA |ENCRYPTED |)PRIVATE KEY-----\s*$"),
    ("aws-access-key-id", r"\bAKIA[0-9A-Z]{16}\b"),
    ("jwt", r"\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\."),
    ("agent-session-url", r"claude\.ai/code/|chatgpt\.com/c/|/sessions?/[0-9a-f-]{20,}"),
    ("releasability-marking", r"^\s*(CUI|UNCLASSIFIED|CONFIDENTIAL|SECRET|TOP SECRET)//|//(NOFORN|REL TO [A-Z]{3})"),
]
SHAPE_RES = [(c, re.compile(p)) for c, p in SHAPES]

# paths whose content is generated or binary-like: a chance substring is not a name
SKIP_SUFFIXES = ("go.sum", "Cargo.lock", "package-lock.json", ".png", ".jpg", ".jpeg", ".gif",
                 ".pdf", ".pmtiles", ".woff", ".woff2", ".ttf", ".min.js")
SKIP_PARTS = ("/web/vendor/",)
B64_CHARS = re.compile(r"[A-Za-z0-9+/=]")


def needle_id(needle: str) -> str:
    return hashlib.sha256(needle.lower().encode("utf-8")).hexdigest()[:12]


def decode_needles(raw: str) -> list:
    blob = base64.b64decode("".join(raw.split()), validate=True)
    if blob[:2] == b"\x1f\x8b":
        blob = gzip.decompress(blob)
    return parse_needles(blob.decode("utf-8"))


def parse_needles(text: str) -> list:
    out = {ln.strip().lower() for ln in text.splitlines()}
    return sorted((n for n in out if len(n) >= 4 and not n.startswith("#")), key=lambda s: (-len(s), s))


def load_needles(path):
    if path:
        with open(path, encoding="utf-8") as fh:
            return parse_needles(fh.read())
    raw = os.environ.get("GOAT_LEAK_NEEDLES", "")
    if not raw.strip():
        return []
    try:
        return decode_needles(raw)
    except (binascii.Error, ValueError, OSError, EOFError, UnicodeDecodeError):
        sys.exit("leak-gate: GOAT_LEAK_NEEDLES does not decode (base64 of a newline-separated list, gzip or plain)")


def added_lines(repo: str, base: str, head: str):
    """Yield (path, line_number, text) for every line the change adds."""
    cmd = ["git", "-C", repo, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
           "--no-renames", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/", f"{base}...{head}"]
    proc = subprocess.run(cmd, capture_output=True)
    if proc.returncode != 0:
        sys.exit("leak-gate: git diff failed: " + proc.stderr.decode("utf-8", "replace")[-400:])
    path, lineno = None, 0
    for raw in proc.stdout.split(b"\n"):
        line = raw.decode("utf-8", "replace")
        if line.startswith("+++ "):
            target = line[4:]
            path = target[2:] if target.startswith("b/") else None
            continue
        if line.startswith("@@ "):
            m = re.match(r"@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@", line)
            lineno = int(m.group(1)) if m else 0
            continue
        if path is None or line.startswith(("--- ", "diff ", "index ", "\\")):
            continue
        if line.startswith("+"):
            yield path, lineno, line[1:]
            lineno += 1


def skipped(path: str) -> bool:
    return path.endswith(SKIP_SUFFIXES) or any(p in "/" + path for p in SKIP_PARTS)


def is_data_line(text: str) -> bool:
    s = text[:20000]
    return len(s) > 4000 and len(B64_CHARS.findall(s)) / len(s) > 0.97


def trie_regex(words) -> str:
    """One pattern matching every word, factored by common prefix so a line is
    tried against a few branches per position instead of every needle. At a
    node where a word ends, longer continuations are tried first."""
    trie: dict = {}
    for w in words:
        node = trie
        for ch in w:
            node = node.setdefault(ch, {})
        node[""] = True

    def emit(node) -> str:
        ends = "" in node
        branches = [re.escape(ch) + emit(child) for ch, child in sorted(node.items()) if ch]
        if not branches:
            return ""
        body = branches[0] if len(branches) == 1 else "(?:" + "|".join(branches) + ")"
        if ends:
            return "(?:" + body + ")?"
        return body
    return emit(trie)


def scan(lines, needles):
    needle_re = re.compile(trie_regex(needles)) if needles else None
    hits, exempt = [], []
    for path, lineno, text in lines:
        if skipped(path):
            continue
        found = []
        if needle_re is not None and not is_data_line(text):
            seen = set()
            for m in needle_re.finditer(text.lower()):
                nid = needle_id(m.group(0))
                if nid not in seen:
                    seen.add(nid)
                    found.append((path, lineno, "derived-token", nid))
        for cls, rx in SHAPE_RES:
            if rx.search(text):
                found.append((path, lineno, cls, ""))
        (exempt if "oss-allow:" in text else hits).extend(found)
    return hits, exempt


def annotation_escape(s: str) -> str:
    return s.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A").replace(":", "%3A").replace(",", "%2C")


def report(result, n_needles) -> int:
    hits, exempt = result
    in_actions = os.environ.get("GITHUB_ACTIONS") == "true"
    for path, lineno, cls, nid in exempt:
        what = f"{cls} needle#{nid}" if nid else cls
        print(f"{path}:{lineno}: exempt by oss-allow: {what}")
        if in_actions:
            print(f"::notice file={annotation_escape(path)},line={lineno},title=leak-gate exemption::{what}")
    for path, lineno, cls, nid in hits:
        what = f"{cls} needle#{nid}" if nid else cls
        print(f"{path}:{lineno}: {what}")
        if in_actions:
            print(f"::error file={annotation_escape(path)},line={lineno},title=leak-gate::{what}")
    print(f"leak-gate: needles={n_needles} hits={len(hits)} exempt={len(exempt)}")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as fh:
            fh.write(f"### leak-gate\n\n{len(hits)} hit(s) and {len(exempt)} `oss-allow` exemption(s) "
                     f"over the added lines, {n_needles} needles.\n\n")
            if hits:
                fh.write("| File | Line | Class |\n|---|---|---|\n")
                for path, lineno, cls, nid in hits:
                    fh.write(f"| `{path}` | {lineno} | {cls}{' needle#' + nid if nid else ''} |\n")
                fh.write("\nA derived-token hit names one of the maintainers' own deployments. "
                         "Replace it with an `*.example` name or the `zagros` example; a maintainer "
                         "can say which value the needle id refers to.\n")
    return 1 if hits else 0


def self_test() -> int:
    needle = "zz-selftest-host.invalid"
    key = "AKIA" + "Z" * 16
    with tempfile.TemporaryDirectory() as d:
        def git(*a):
            subprocess.run(["git", "-C", d, "-c", "user.name=t", "-c", "user.email=t@example.com",
                            "-c", "commit.gpgsign=false", *a], check=True, capture_output=True)
        git("init", "-q", "-b", "main")
        with open(os.path.join(d, "a.txt"), "w") as fh:
            fh.write("one\n" + needle + " was here before\n")
        git("add", "-A"); git("commit", "-q", "-m", "base")
        base = subprocess.run(["git", "-C", d, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        with open(os.path.join(d, "a.txt"), "a") as fh:
            fh.write("clean\nhost " + needle.upper() + " added\n" + key + " oss-allow: fixture\n" + key + "\n"
                     + needle + " # oss-allow: fixture\n"
                     + '"-----BEGIN ' + 'PRIVATE KEY-----\\n"\n' + "-----BEGIN " + "PRIVATE KEY-----\n")
        with open(os.path.join(d, "go.sum"), "w") as fh:
            fh.write(needle + "\n")
        git("add", "-A"); git("commit", "-q", "-m", "head")
        head = subprocess.run(["git", "-C", d, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        env = dict(os.environ, GOAT_LEAK_NEEDLES=base64.b64encode(gzip.compress((needle + "\n#comment\n").encode())).decode())
        env.pop("GITHUB_ACTIONS", None); env.pop("GITHUB_STEP_SUMMARY", None)
        r = subprocess.run([sys.executable, __file__, "--repo", d, "--base", base, "--head", head],
                           capture_output=True, text=True, env=env)
        out = r.stdout + r.stderr
        want = [f"a.txt:4: derived-token needle#{needle_id(needle)}", "a.txt:6: aws-access-key-id",
                "a.txt:5: exempt by oss-allow: aws-access-key-id",
                f"a.txt:7: exempt by oss-allow: derived-token needle#{needle_id(needle)}",
                "a.txt:9: pem-private", "hits=3 exempt=2"]
        problems = [f"missing {w!r}" for w in want if w not in out]
        if needle.lower() in out.lower() or key in out:
            problems.append("a matched value was printed")
        if r.returncode != 1:
            problems.append(f"exit {r.returncode}, want 1")
        env.pop("GOAT_LEAK_NEEDLES")
        r2 = subprocess.run([sys.executable, __file__, "--repo", d, "--base", base, "--head", head],
                            capture_output=True, text=True, env=env)
        if r2.returncode != 2:
            problems.append(f"no needle list: exit {r2.returncode}, want 2")
    if problems:
        print("self-test FAILED: " + "; ".join(problems))
        return 1
    print("self-test ok")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--base")
    ap.add_argument("--head")
    ap.add_argument("--repo", default=".")
    ap.add_argument("--needles-file")
    ap.add_argument("--self-test", action="store_true")
    a = ap.parse_args()
    if a.self_test:
        return self_test()
    if not (a.base and a.head):
        ap.error("--base and --head are required")
    needles = load_needles(a.needles_file)
    if not needles:
        print("leak-gate: no needle list. The repository secret GOAT_LEAK_NEEDLES is unset or empty; "
              "an owner sets it. The gate fails closed.", file=sys.stderr)
        return 2
    return report(scan(added_lines(a.repo, a.base, a.head), needles), len(needles))


if __name__ == "__main__":
    sys.exit(main())
