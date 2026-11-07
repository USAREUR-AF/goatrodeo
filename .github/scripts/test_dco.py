#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Offline DCO failure controls; no network or unsigned Git commits are created."""
import contextlib
import io
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("dco", Path(__file__).with_name("check_dco.py"))
dco = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dco)


class Trailers(unittest.TestCase):
    def test_matching_trailer(self):
        self.assertTrue(dco.signed_off("Contributor", "person@example.org",
                        "fix: fixture\n\nSigned-off-by: Contributor <person@example.org>\n"))

    def test_body_mention_is_not_a_trailer(self):
        self.assertFalse(dco.signed_off("Contributor", "person@example.org",
                         "fix: fixture\n\nExample: Signed-off-by: Contributor <person@example.org>\n\nFurther explanation.\n"))

    def test_other_author_is_rejected(self):
        self.assertFalse(dco.signed_off("Contributor", "person@example.org",
                         "fix: fixture\n\nSigned-off-by: Someone Else <other@example.org>\n"))

    def test_noreply_alias_does_not_prove_address_ownership(self):
        self.assertFalse(dco.signed_off("Contributor", "123+person@users.noreply.github.com",
                         "fix: fixture\n\nSigned-off-by: person <someone@example.org>\n"))


class Ranges(unittest.TestCase):
    def setUp(self):
        capture = contextlib.redirect_stdout(io.StringIO())
        capture.__enter__()
        self.addCleanup(capture.__exit__, None, None, None)

    def test_unresolvable_range_is_not_success(self):
        with patch.object(dco, "git", side_effect=subprocess.CalledProcessError(128, ["git"])):
            with self.assertRaises(subprocess.CalledProcessError):
                dco.check("a" * 40, "b" * 40, "person")

    def test_invalid_endpoints_are_refused_before_git(self):
        with patch.object(dco, "git") as git:
            with self.assertRaises(ValueError):
                dco.check("--all", "b" * 40, "person")
            git.assert_not_called()

    def test_all_commits_are_checked_including_merges(self):
        with patch.object(dco, "git", side_effect=["c\nd\n", "A\na@example.org\nbody", "B\nb@example.org\nbody"]) as git:
            with patch.object(dco, "signed_off", side_effect=[True, False]):
                self.assertEqual(dco.check("a" * 40, "b" * 40, "person"), 1)
            self.assertEqual(git.call_args_list[0].args, ("rev-list", "a" * 40 + ".." + "b" * 40))

    def test_bot_email_on_human_pr_is_not_exempt(self):
        info = "dependabot[bot]\n49699333+dependabot[bot]@users.noreply.github.com\nbody"
        with patch.object(dco, "git", side_effect=["c\n", info]), patch.object(dco, "signed_off", return_value=False):
            self.assertEqual(dco.check("a" * 40, "b" * 40, "person"), 1)

    def test_authentic_bot_pr_exception(self):
        info = "dependabot[bot]\n49699333+dependabot[bot]@users.noreply.github.com\nbody"
        with patch.object(dco, "git", side_effect=["c\n", info]), patch.object(dco, "signed_off") as trailers:
            self.assertEqual(dco.check("a" * 40, "b" * 40, "dependabot[bot]"), 0)
            trailers.assert_not_called()


if __name__ == "__main__":
    unittest.main()
