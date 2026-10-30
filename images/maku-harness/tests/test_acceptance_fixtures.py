"""HR3: the judge's acceptance reader and the factory's scoper reader agree on one fixture set. The Go
twin is internal/factory/reconciler's TestAcceptanceFixturesAgreeWithTheHarness, reading this directory."""
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, ".."))
import loop_policy as lp  # noqa: E402

FIXTURES = os.path.join(HERE, "acceptance")


class AcceptanceFixturesTest(unittest.TestCase):
    def test_each_fixture_reads_as_expected(self):
        with open(os.path.join(FIXTURES, "expected.json"), encoding="utf-8") as f:
            expected = json.load(f)
        self.assertGreaterEqual(len(expected), 5)
        for name, want in expected.items():
            with self.subTest(name), open(os.path.join(FIXTURES, name), encoding="utf-8") as f:
                task = f.read()
                got = lp.acceptance(task)
                if want["text"] is None:  # nothing stated: the judge falls through to the whole brief
                    self.assertFalse(want["states"])
                    self.assertEqual(got, task.strip()[: lp.ACCEPTANCE_MAX])
                else:
                    self.assertTrue(want["states"])
                    self.assertEqual(got, want["text"])

    def test_an_inline_paragraph_is_bounded(self):
        self.assertEqual(len(lp.acceptance("Acceptance: " + "x" * 10000)), lp.ACCEPTANCE_MAX)


if __name__ == "__main__":
    unittest.main()
