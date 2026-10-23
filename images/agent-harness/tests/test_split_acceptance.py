"""The splitter's part brief (splitter design SL9, offline proof 7): the judge's acceptance() reads the
part's checks, never the whole issue's. The brief is the factory's golden render, written by
internal/factory/reconciler/split_brief_test.go (UPDATE_GOLDEN=1)."""
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, ".."))
import loop_policy as lp  # noqa: E402

BRIEF = os.path.join(HERE, "testdata", "split_child_brief.txt")


class PartAcceptanceTest(unittest.TestCase):
    def test_the_judge_reads_the_parts_checks(self):
        with open(BRIEF, encoding="utf-8") as f:
            checks = lp.acceptance(f.read())
        self.assertIn("task docs (CI: validate): proves every relref resolves", checks)
        self.assertNotIn("the whole issue's check", checks)
        self.assertNotIn("Touches", checks)


if __name__ == "__main__":
    unittest.main()
