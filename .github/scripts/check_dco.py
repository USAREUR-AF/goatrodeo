#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Check actual DCO trailers for every commit introduced by a pull request."""
import os
import re
import subprocess
import sys


def git(*args, text=None):
    return subprocess.run(["git", *args], input=text, capture_output=True,
                          text=True, check=True).stdout


def signed_off(name, email, body):
    trailers = git("interpret-trailers", "--parse", text=body)
    expected = f"Signed-off-by: {name} <{email}>".casefold()
    return expected in (line.strip().casefold() for line in trailers.splitlines())


def check(base, head, opener):
    for sha in (base, head):
        if not re.fullmatch(r"[0-9a-fA-F]{40}", sha):
            raise ValueError("Both pull request endpoints must be full commit SHAs")
    # Resolve the range before the loop: a Git failure must never become an empty pass.
    commits = git("rev-list", f"{base}..{head}").splitlines()
    failed = []
    for sha in commits:
        name, email, body = git("show", "-s", "--format=%an%n%ae%n%B", sha).split("\n", 2)
        # Scope the automation exception to the authenticated PR opener and bot identity.
        if (opener == "dependabot[bot]" and name == "dependabot[bot]"
                and email == "49699333+dependabot[bot]@users.noreply.github.com"):
            continue
        if not signed_off(name, email, body):
            failed.append(sha)
    for sha in failed:
        print(f"::error::Commit {sha} needs an author-matching Signed-off-by trailer.")
    return 1 if failed else 0


def main():
    try:
        return check(os.environ.get("BASE_SHA", ""), os.environ.get("HEAD_SHA", ""),
                     os.environ.get("PR_USER", ""))
    except (subprocess.CalledProcessError, ValueError) as error:
        print(f"::error::DCO validation could not complete ({type(error).__name__}).", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
