"""Unit tests for the prt-transcoder master shim.

The shim is an extensionless executable under bin/, so it is loaded by path
under a non-`__main__` module name (which keeps the `if __name__ ...` guard
from running main() on import). Pure-stdlib unittest -- no third-party dep, in
keeping with the tool's pure-stdlib design; runnable via either
`python3 -m unittest` or `python3 -m pytest`.
"""

from __future__ import annotations

import importlib.machinery
import importlib.util
import os
import unittest
from pathlib import Path
from unittest import mock

_SHIM_PATH = Path(__file__).resolve().parent.parent / "bin" / "prt-transcoder"


def _load_shim():
    loader = importlib.machinery.SourceFileLoader("prt_transcoder", str(_SHIM_PATH))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


prt = _load_shim()


class EvaluateOutcomeTests(unittest.TestCase):
    """_evaluate_outcome is the pure core of the R1 fallback decision."""

    def test_success_no_fallback(self):
        self.assertEqual(prt._evaluate_outcome(0, False), ("success", False))

    def test_cancelled_never_falls_back(self):
        # Plex stopped the job: a nonzero rc is expected and must NOT re-run.
        self.assertEqual(prt._evaluate_outcome(255, True), ("cancelled", False))
        self.assertEqual(prt._evaluate_outcome(143, True), ("cancelled", False))
        # A clean exit during teardown is still classed as cancelled.
        self.assertEqual(prt._evaluate_outcome(0, True), ("cancelled", False))

    def test_version_skew(self):
        self.assertEqual(
            prt._evaluate_outcome(prt.VERSION_SKEW_RC, False),
            ("version_skew", True),
        )

    def test_transport_error(self):
        self.assertEqual(
            prt._evaluate_outcome(prt.SSH_TRANSPORT_RC, False),
            ("transport_error", True),
        )

    def test_generic_worker_error_falls_back(self):
        # The whole point of R1: any non-cancellation nonzero rc rescues.
        for rc in (1, 2, 69, 137):
            with self.subTest(rc=rc):
                self.assertEqual(
                    prt._evaluate_outcome(rc, False), ("worker_error", True)
                )

    def test_sentinel_codes_are_distinct(self):
        self.assertNotEqual(prt.VERSION_SKEW_RC, prt.SSH_TRANSPORT_RC)


class EnvForwardingTests(unittest.TestCase):
    def test_expected_version_is_forwarded(self):
        self.assertTrue(prt._should_forward("PRT_EXPECTED_PLEX_VERSION"))

    def test_unrelated_var_not_forwarded(self):
        self.assertFalse(prt._should_forward("SECRET_FOO"))
        self.assertFalse(prt._should_forward("AWS_SECRET_ACCESS_KEY"))

    def test_existing_prefixes_still_forwarded(self):
        self.assertTrue(prt._should_forward("PLEX_MEDIA_SERVER_HOME"))
        self.assertTrue(prt._should_forward("X_PLEX_TOKEN"))
        self.assertTrue(prt._should_forward("FFMPEG_EXTERNAL_LIBS"))

    def test_other_prt_vars_not_forwarded(self):
        # We allow the exact name only, not a PRT_ prefix, to stay bounded.
        self.assertFalse(prt._should_forward("PRT_SOMETHING_ELSE"))


class BuildRemoteCommandTests(unittest.TestCase):
    def test_includes_expected_version_and_shape(self):
        fake_env = {
            "PRT_EXPECTED_PLEX_VERSION": "1.43.2.10687-563d026ea",
            "SECRET_FOO": "leak",
            "LANG": "en_US.UTF-8",
        }
        with mock.patch.dict(os.environ, fake_env, clear=True), mock.patch.object(
            prt.os, "getcwd", return_value="/var/lib/transcode"
        ), mock.patch.object(prt.sys, "argv", ["prt-transcoder", "-i", "in.mkv"]):
            cmd = prt._build_remote_command("/usr/lib/plexmediaserver/Plex Transcoder")

        self.assertIn("PRT_EXPECTED_PLEX_VERSION=1.43.2.10687-563d026ea", cmd)
        self.assertNotIn("SECRET_FOO", cmd)
        self.assertTrue(cmd.startswith("cd /var/lib/transcode && exec env -i "))
        self.assertIn("-- ", cmd)
        self.assertIn("in.mkv", cmd)


if __name__ == "__main__":
    unittest.main()
