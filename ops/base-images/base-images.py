#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""base-images.py — the base images every goat image builds on, by role.

Every Dockerfile in this tree takes its base images as build arguments named
BASE_<ROLE> (BASE_RUST_BUILD, BASE_RUNTIME_CC, ...). Each role is one file,
ops/base-images/<role>/Dockerfile, holding a single digest-pinned FROM line:
that line is the role's image. The files in this repository name images
anyone may pull and redistribute. An environment that wants other bases (a
hardened vendor catalogue, its own mirror) puts its own file at the same path
in its overlay; the overlay is copied over this tree before a build, so the
build reads the overlay's line instead. Design: goatcore docs/design/base-images.md.
How to do it for your own environment: goatcore docs/operations/build-on-your-own-base-images.md.

  list   [--root DIR]
         Print each role, its build argument and its image.
  args   --dockerfile FILE [--target STAGE] [--root DIR] [--github-output]
         Print BASE_<ROLE>=<image> for every base argument FILE declares, from
         the role files under DIR (default: this tree). With --github-output,
         also write build_args (multiline) and base_name (the image the final
         or --target stage starts from) to $GITHUB_OUTPUT.
  build  --unit STEM --tag REF [--platform P] [--push] [--root DIR]
         Build one goat image on the role files under DIR with docker buildx:
         the context, Dockerfile and target come from the image's workflow
         .github/workflows/STEM.yml, the base arguments from `args`. Run it
         in an assembled tree (goatcore or goatstuff with your overlay copied
         over it) to build an image on your own bases.
  sync   [--root DIR]
         Rewrite every Dockerfile's `ARG BASE_<ROLE>=` default to the role's
         image, so a plain `docker build` uses the same bases CI does. Run it
         after a role file changes (a Dependabot refresh, a version move).
  check  [--root DIR]
         The public tree's gate: role files are single digest-pinned FROM lines
         on a registry the public can use; every Dockerfile names its bases
         through a role and its defaults match the role files; no Dockerfile,
         role default or compose template names a subscription registry
         except the references waived in ops/base-images/vendor-refs.allow.
  --self-test
         Prove the gate red on each defect it exists to catch, green on a
         clean tree.
"""

import os
import re
import subprocess
import sys
import tempfile

ROLE_DIR = os.path.join("ops", "base-images")
ALLOW = os.path.join(ROLE_DIR, "vendor-refs.allow")

# Registries whose images the public tree may not default to: pulling from
# them needs a subscription or an account, and their terms do not cover
# redistribution of images built on them. An overlay may use them freely.
SUBSCRIPTION_REGISTRIES = ("cgr.dev/", "registry1.dso.mil/")

DIGEST = re.compile(r"@sha256:[0-9a-f]{64}$")
ARG_RE = re.compile(r"^\s*ARG\s+(BASE_[A-Z0-9_]+)(?:=(\S*))?\s*$")
FROM_RE = re.compile(r"^\s*FROM\s+(.*)$", re.I)
VAR_RE = re.compile(r"^\$\{?(BASE_[A-Z0-9_]+)\}?$")
IMAGE_KEY_RE = re.compile(r"^\s*(?:-\s*)?([A-Za-z0-9_]*image[A-Za-z0-9_]*)\s*:\s*(.+)$")


def default_root():
    return os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))


def arg_name(role):
    return "BASE_" + role.upper().replace("-", "_")


def is_dockerfile(path):
    b = os.path.basename(path)
    return b == "Dockerfile" or b.startswith("Dockerfile.") or b.endswith(".Dockerfile")


def subscription(ref):
    return ref.startswith(SUBSCRIPTION_REGISTRIES)


def files(root):
    """Tracked files when root is a git checkout (what CI sees), else a walk."""
    try:
        r = subprocess.run(["git", "-C", root, "ls-files", "-z"], capture_output=True, check=True)
        listed = [x for x in r.stdout.decode().split("\0") if x]
        if listed:
            return sorted(x for x in listed if os.path.isfile(os.path.join(root, x)))
    except (OSError, subprocess.CalledProcessError):
        pass
    out = []
    for d, dirs, fs in os.walk(root):
        dirs[:] = sorted(x for x in dirs if x not in (".git", "node_modules", "target"))
        for f in sorted(fs):
            out.append(os.path.relpath(os.path.join(d, f), root))
    return out


def from_lines(text):
    """(line number, image, stage alias or None) for each FROM."""
    out = []
    for n, ln in enumerate(text.splitlines(), 1):
        m = FROM_RE.match(ln)
        if not m:
            continue
        toks = [t for t in m.group(1).split() if not t.startswith("--")]
        if not toks:
            continue
        alias = toks[2].lower() if len(toks) >= 3 and toks[1].lower() == "as" else None
        out.append((n, toks[0], alias))
    return out


def read_roles(root):
    """{role: (image, path)}; raises ValueError naming every malformed role file."""
    base = os.path.join(root, ROLE_DIR)
    roles, problems = {}, []
    if not os.path.isdir(base):
        return roles
    for role in sorted(os.listdir(base)):
        p = os.path.join(base, role, "Dockerfile")
        if not os.path.isfile(p):
            continue
        with open(p, encoding="utf-8") as fh:
            froms = from_lines(fh.read())
        rel = os.path.relpath(p, root)
        if len(froms) != 1:
            problems.append(f"{rel}: holds {len(froms)} FROM lines; a role file holds exactly one")
            continue
        roles[role] = (froms[0][1], rel)
    if problems:
        raise ValueError("\n".join(problems))
    return roles


def declared_args(text):
    """{BASE_X: default or None} for ARGs before the first FROM (global scope)."""
    out = {}
    for ln in text.splitlines():
        if FROM_RE.match(ln):
            break
        m = ARG_RE.match(ln)
        if m:
            out[m.group(1)] = m.group(2)
    return out


def used_args(text):
    used = []
    for n, img, _ in from_lines(text):
        m = VAR_RE.match(img)
        if m:
            used.append((n, m.group(1)))
    return used


def by_arg(roles):
    return {arg_name(r): (img, path, r) for r, (img, path) in roles.items()}


GLOBAL_ARG_RE = re.compile(r"^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)=(\S+)\s*$")


def expand(img, text, args):
    """Substitute global ARG defaults (and the base arguments) into an image ref."""
    env = {}
    for ln in text.splitlines():
        if FROM_RE.match(ln):
            break
        m = GLOBAL_ARG_RE.match(ln)
        if m:
            env[m.group(1)] = m.group(2)
    env.update(args)
    return re.sub(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?", lambda m: env.get(m.group(1), m.group(0)), img)


def resolve_base(text, args, target=None):
    """The image the final stage (or --target) starts from, following aliases."""
    froms = from_lines(text)
    if not froms:
        return ""
    stages = {a: img for _, img, a in froms if a}
    img = stages.get(target.lower(), "") if target else froms[-1][1]
    for _ in range(len(froms) + 1):
        m = VAR_RE.match(img)
        if m:
            return args.get(m.group(1), img)
        if img.lower() in stages:
            img = stages[img.lower()]
            continue
        return expand(img, text, args)
    return expand(img, text, args)


# ---------------------------------------------------------------- commands

def cmd_list(root):
    roles = read_roles(root)
    if not roles:
        print(f"no role files under {os.path.join(root, ROLE_DIR)}")
        return 1
    w = max(len(r) for r in roles)
    for r, (img, _) in roles.items():
        print(f"{r:<{w}}  {arg_name(r):<{w + 5}}  {img}")
    return 0


def cmd_args(root, dockerfile, target, github_output):
    roles = by_arg(read_roles(root))
    if not os.path.isfile(dockerfile):
        print(f"::error::no Dockerfile at {dockerfile}", file=sys.stderr)
        return 1
    with open(dockerfile, encoding="utf-8") as fh:
        text = fh.read()
    declared = declared_args(text)
    missing = sorted(a for a in declared if a not in roles)
    if missing:
        for a in missing:
            print(f"::error::{dockerfile} declares {a}, and {os.path.join(root, ROLE_DIR)} has no role for it", file=sys.stderr)
        return 1
    values = {a: roles[a][0] for a in sorted(declared)}
    lines = [f"{a}={v}" for a, v in values.items()]
    for ln in lines:
        print(ln)
    if github_output:
        out = os.environ.get("GITHUB_OUTPUT")
        if not out:
            print("::error::--github-output needs $GITHUB_OUTPUT", file=sys.stderr)
            return 1
        with open(out, "a", encoding="utf-8") as fh:
            fh.write("build_args<<GOAT_BASE_IMAGES_EOF\n")
            fh.write("".join(ln + "\n" for ln in lines))
            fh.write("GOAT_BASE_IMAGES_EOF\n")
            fh.write(f"base_name={resolve_base(text, values, target)}\n")
    return 0


def unit_inputs(root, unit):
    """context, file, target and image_name from an image workflow stub."""
    p = os.path.join(root, ".github", "workflows", unit + ".yml")
    if not os.path.isfile(p):
        raise ValueError(f"no image workflow {os.path.relpath(p, root)}; units are the *-image.yml stems there")
    got, after = {"context": ".", "file": "", "target": "", "image_name": unit}, False
    with open(p, encoding="utf-8") as fh:
        for ln in fh:
            if re.match(r"^\s*uses:\s*\./\.github/workflows/_image-build\.yml", ln):
                after = True
                continue
            m = re.match(r"^\s*(context|file|target|image_name):\s*['\"]?([^'\"#\s]*)", ln)
            if after and m:
                got[m.group(1)] = m.group(2)
    if not after:
        raise ValueError(f"{os.path.relpath(p, root)} does not build through _image-build.yml; read its own build step")
    return got


def cmd_build(root, unit, tag, platform, push):
    inp = unit_inputs(root, unit)
    dockerfile = os.path.join(root, inp["file"] or os.path.join(inp["context"], "Dockerfile"))
    roles = by_arg(read_roles(root))
    with open(dockerfile, encoding="utf-8") as fh:
        declared = declared_args(fh.read())
    missing = sorted(a for a in declared if a not in roles)
    if missing:
        raise ValueError(f"{dockerfile} declares {', '.join(missing)} with no role file")
    cmd = ["docker", "buildx", "build", "-f", dockerfile]
    for a in sorted(declared):
        cmd += ["--build-arg", f"{a}={roles[a][0]}"]
    if inp["target"]:
        cmd += ["--target", inp["target"]]
    if platform:
        cmd += ["--platform", platform]
    cmd += ["-t", tag, "--push" if push else "--load", os.path.join(root, inp["context"])]
    print("+ " + " ".join(cmd), flush=True)
    return subprocess.call(cmd)


def rewrite_defaults(text, roles):
    out, changed = [], False
    first_from = False
    for ln in text.splitlines(keepends=True):
        if FROM_RE.match(ln):
            first_from = True
        m = ARG_RE.match(ln.rstrip("\n"))
        if m and not first_from and m.group(1) in roles:
            want = f"ARG {m.group(1)}={roles[m.group(1)][0]}\n"
            if ln != want:
                ln, changed = want, True
        out.append(ln)
    return "".join(out), changed


def cmd_sync(root):
    roles = by_arg(read_roles(root))
    n = 0
    for rel in files(root):
        if not is_dockerfile(rel) or rel.startswith(ROLE_DIR + os.sep):
            continue
        p = os.path.join(root, rel)
        with open(p, encoding="utf-8") as fh:
            text = fh.read()
        new, changed = rewrite_defaults(text, roles)
        if changed:
            with open(p, "w", encoding="utf-8") as fh:
                fh.write(new)
            print(f"synced  {rel}")
            n += 1
    print(f"{n} Dockerfile(s) rewritten")
    return 0


def load_allow(root):
    allow = {}
    p = os.path.join(root, ALLOW)
    if not os.path.exists(p):
        return allow
    with open(p, encoding="utf-8") as fh:
        for n, ln in enumerate(fh, 1):
            body = ln.split("#", 1)[0].strip()
            if body:
                path, _, key = body.partition(" ")
                allow[(path, key.strip())] = n
    return allow


def clean(v):
    v = re.sub(r"\s+#.*$", "", v).strip()
    return v.strip("'\"").strip()


def cmd_check(root):
    fail = []
    try:
        roles = read_roles(root)
    except ValueError as e:
        print(f"ROLE FILE  {e}")
        return 1
    if not roles:
        print(f"no role files under {ROLE_DIR}; nothing defines the base images")
        return 1
    for r, (img, path) in roles.items():
        if not DIGEST.search(img):
            fail.append(f"ROLE NOT PINNED  {path}\n    {img}\n    Pin the multi-arch index digest: `crane digest <ref>` prints it.")
        if subscription(img):
            fail.append(f"ROLE ON A SUBSCRIPTION REGISTRY  {path}\n    {img}\n"
                        "    A public default must be an image anyone may pull and redistribute.\n"
                        "    Put this line in your overlay's copy of the file instead (goatcore docs/operations/build-on-your-own-base-images.md).")
    args = by_arg(roles)
    allow = load_allow(root)
    seen = set()
    stale_sync = []
    for rel in files(root):
        p = os.path.join(root, rel)
        if is_dockerfile(rel) and not rel.startswith(ROLE_DIR + os.sep):
            with open(p, encoding="utf-8", errors="replace") as fh:
                text = fh.read()
            declared = declared_args(text)
            for a, default in declared.items():
                if a not in args:
                    fail.append(f"NO SUCH ROLE  {rel}\n    ARG {a} has no file {ROLE_DIR}/<role>/Dockerfile; roles are: "
                                + ", ".join(sorted(args)))
                elif default != args[a][0]:
                    stale_sync.append(rel)
            for n, a in used_args(text):
                if a not in declared:
                    fail.append(f"UNDECLARED BASE  {rel}:{n}\n    FROM ${{{a}}} with no `ARG {a}=<default>` before the first FROM.")
            for n, img, _ in from_lines(text):
                if subscription(img):
                    key = img.split("@")[0]
                    seen.add((rel, key))
                    if (rel, key) not in allow:
                        fail.append(f"SUBSCRIPTION BASE  {rel}:{n}\n    FROM {img}\n"
                                    "    Name the base through a role: `ARG BASE_<ROLE>=` before the first FROM and\n"
                                    "    `FROM ${BASE_<ROLE>}`, then run ops/base-images/base-images.py sync.")
        elif rel.startswith("ops/ansible/") and rel.endswith((".yml", ".yaml", ".j2")):
            with open(p, encoding="utf-8", errors="replace") as fh:
                for n, ln in enumerate(fh, 1):
                    if ln.lstrip().startswith("#"):
                        continue
                    m = IMAGE_KEY_RE.match(ln)
                    if not m:
                        continue
                    key, val = m.group(1), clean(m.group(2))
                    if not subscription(val):
                        continue
                    k = key if key != "image" else val.split("@")[0]
                    seen.add((rel, k))
                    if (rel, k) not in allow:
                        fail.append(f"SUBSCRIPTION IMAGE  {rel}:{n}\n    {key}: {val}\n"
                                    "    A public default must be an image anyone may pull. Set this one in your\n"
                                    "    overlay's group_vars, or waive it with a reason in " + ALLOW + ".")
    for rel in sorted(set(stale_sync)):
        fail.append(f"DEFAULT OUT OF SYNC  {rel}\n    An `ARG BASE_<ROLE>=` default differs from its role file.\n"
                    "    Fix: python3 ops/base-images/base-images.py sync")
    for (path, key), n in sorted(allow.items(), key=lambda x: x[1]):
        if (path, key) not in seen:
            fail.append(f"STALE ALLOW  {ALLOW}:{n}\n    {path} {key}\n    This reference is gone. Delete the line; the list only shrinks.")
    if fail:
        print("\n".join(fail))
        print(f"\n{len(fail)} problem(s). Design: goatcore docs/design/base-images.md")
        return 1
    print(f"ok: {len(roles)} role(s), every one pinned and public; every Dockerfile names its bases through a role; "
          f"{len(allow)} waived reference(s)")
    return 0


# ---------------------------------------------------------------- self-test

def self_test():
    d64 = "a" * 64
    pub = f"docker.io/library/rust:1.92-bookworm@sha256:{d64}"
    run = f"gcr.io/distroless/cc-debian13:nonroot@sha256:{d64}"

    def tree(tmp, role_rust=pub, svc=None, compose=None, allow=None):
        for role, img in (("rust-build", role_rust), ("runtime-cc", run)):
            os.makedirs(os.path.join(tmp, ROLE_DIR, role), exist_ok=True)
            with open(os.path.join(tmp, ROLE_DIR, role, "Dockerfile"), "w") as fh:
                fh.write(f"# role\nFROM {img}\n")
        os.makedirs(os.path.join(tmp, "svc"), exist_ok=True)
        with open(os.path.join(tmp, "svc", "Dockerfile"), "w") as fh:
            fh.write(svc if svc is not None else
                     f"ARG BASE_RUST_BUILD={role_rust}\nARG BASE_RUNTIME_CC={run}\n"
                     "FROM ${BASE_RUST_BUILD} AS build\nFROM ${BASE_RUNTIME_CC}\n")
        os.makedirs(os.path.join(tmp, "ops", "ansible", "roles", "fx", "defaults"), exist_ok=True)
        with open(os.path.join(tmp, "ops", "ansible", "roles", "fx", "defaults", "main.yml"), "w") as fh:
            fh.write(compose if compose is not None else f'fx_image: "ghcr.io/x/fx@sha256:{d64}"\n')
        p = os.path.join(tmp, ALLOW)
        if allow is not None:
            with open(p, "w") as fh:
                fh.write(allow)
        elif os.path.exists(p):
            os.remove(p)

    def verdict(tmp):
        devnull = open(os.devnull, "w")
        old = sys.stdout
        sys.stdout = devnull
        try:
            return cmd_check(tmp)
        finally:
            sys.stdout = old
            devnull.close()

    cases = [
        ("a role on a subscription registry", dict(role_rust=f"cgr.dev/chainguard/rust:latest-dev@sha256:{d64}")),
        ("a role pinned by tag only", dict(role_rust="docker.io/library/rust:1.92-bookworm")),
        ("a Dockerfile FROM on a subscription registry",
         dict(svc=f"FROM cgr.dev/chainguard/glibc-dynamic:latest@sha256:{d64}\n")),
        ("an ARG default that differs from its role file",
         dict(svc=f"ARG BASE_RUNTIME_CC=gcr.io/distroless/cc-debian12@sha256:{d64}\nFROM ${{BASE_RUNTIME_CC}}\n")),
        ("an ARG naming a role that does not exist",
         dict(svc=f"ARG BASE_NOPE=x@sha256:{d64}\nFROM ${{BASE_NOPE}}\n")),
        ("a FROM on a base argument never declared", dict(svc="FROM ${BASE_RUNTIME_CC}\n")),
        ("a role default on a subscription registry",
         dict(compose=f'fx_image: "cgr.dev/chainguard/minio:latest@sha256:{d64}"\n')),
        ("an allow line whose reference is gone", dict(allow="ops/ansible/roles/gone/defaults/main.yml gone_image  # fx\n")),
    ]
    with tempfile.TemporaryDirectory() as tmp:
        tree(tmp)
        if verdict(tmp) != 0:
            print("SELF-TEST FAILED: the gate rejected a clean tree.")
            return 1
        for what, kw in cases:
            tree(tmp, **kw)
            if verdict(tmp) == 0:
                print(f"SELF-TEST FAILED: the gate passed {what}.")
                return 1
        tree(tmp, compose=f'fx_image: "cgr.dev/chainguard/minio:latest@sha256:{d64}"\n',
             allow="ops/ansible/roles/fx/defaults/main.yml fx_image  # no public build exists\n")
        if verdict(tmp) != 0:
            print("SELF-TEST FAILED: the gate rejected a waived reference.")
            return 1
        # sync repairs a stale default; args reads an overlay's role file.
        tree(tmp, svc=f"ARG BASE_RUNTIME_CC=old@sha256:{d64}\nFROM ${{BASE_RUNTIME_CC}}\n")
        devnull = open(os.devnull, "w")
        old, sys.stdout = sys.stdout, devnull
        try:
            cmd_sync(tmp)
        finally:
            sys.stdout = old
            devnull.close()
        if verdict(tmp) != 0:
            print("SELF-TEST FAILED: sync did not repair a stale default.")
            return 1
        vendor = f"cgr.dev/chainguard/glibc-dynamic:latest@sha256:{d64}"
        with open(os.path.join(tmp, ROLE_DIR, "runtime-cc", "Dockerfile"), "w") as fh:
            fh.write(f"FROM {vendor}\n")
        with open(os.path.join(tmp, "svc", "Dockerfile"), encoding="utf-8") as fh:
            text = fh.read()
        values = {a: v[0] for a, v in by_arg(read_roles(tmp)).items() if a in declared_args(text)}
        if values.get("BASE_RUNTIME_CC") != vendor or resolve_base(text, values) != vendor:
            print("SELF-TEST FAILED: args did not take the overlay's role file.")
            return 1
    print("self-test ok: red on a subscription role, an unpinned role, a subscription FROM, a stale default, "
          "an unknown role, an undeclared base, a subscription role default and a stale allow line; "
          "green on a clean tree and a waived reference; sync repairs; args follows the overlay")
    return 0


def main(argv):
    if not argv or argv[0] in ("-h", "--help"):
        print(__doc__)
        return 0
    if argv[0] == "--self-test":
        return self_test()
    cmd, rest = argv[0], argv[1:]
    opts = {"--root": default_root(), "--dockerfile": None, "--target": None,
            "--unit": None, "--tag": None, "--platform": None}
    flags = set()
    i = 0
    while i < len(rest):
        a = rest[i]
        if a in opts:
            if i + 1 >= len(rest):
                print(f"{a} needs a value", file=sys.stderr)
                return 2
            opts[a] = rest[i + 1]
            i += 2
        elif a in ("--github-output", "--push"):
            flags.add(a)
            i += 1
        else:
            print(f"unknown argument {a}", file=sys.stderr)
            return 2
    root = os.path.abspath(opts["--root"])
    try:
        if cmd == "list":
            return cmd_list(root)
        if cmd == "args":
            if not opts["--dockerfile"]:
                print("args needs --dockerfile", file=sys.stderr)
                return 2
            return cmd_args(root, opts["--dockerfile"], opts["--target"] or None, "--github-output" in flags)
        if cmd == "build":
            if not opts["--unit"] or not opts["--tag"]:
                print("build needs --unit and --tag", file=sys.stderr)
                return 2
            return cmd_build(root, opts["--unit"], opts["--tag"], opts["--platform"], "--push" in flags)
        if cmd == "sync":
            return cmd_sync(root)
        if cmd == "check":
            return cmd_check(root)
    except ValueError as e:
        print(f"::error::{e}", file=sys.stderr)
        return 1
    print(f"unknown command {cmd}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
