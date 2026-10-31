"""harness-checks' verdict (tests/checks/verdict.py): which non-passing steps are the harness's
fault, and which are the checked repository's own verdict on its main."""
import os
import sys
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "checks"))
from verdict import classify  # noqa: E402

STOPPED = "an earlier step stopped the run"


def step(name, result, reason=None):
    s = {"name": name, "result": result, "seconds": 0}
    if reason:
        s["reason"] = reason
    return s


class Classify(unittest.TestCase):
    def test_all_pass_is_clean(self):
        self.assertEqual(classify([step("a", "pass"), step("b", "pass")]), ([], []))

    def test_a_declared_skip_is_clean(self):
        self.assertEqual(classify([step("a", "pass"), step("b", "skip")]), ([], []))

    def test_a_failing_step_is_the_repositorys_and_so_are_the_steps_it_stopped(self):
        steps = [step("overlay", "fail"), step("suites", "skip", STOPPED), step("links", "skip", STOPPED)]
        self.assertEqual(classify(steps), (["overlay"], []))

    def test_a_timeout_is_the_harnesss(self):
        self.assertEqual(classify([step("a", "timeout")]), ([], ["a"]))

    def test_a_spent_budget_is_the_harnesss_even_after_a_fail(self):
        steps = [step("a", "fail"), step("b", "skip", "the budget is spent")]
        self.assertEqual(classify(steps), (["a"], ["b"]))

    def test_a_stop_with_no_failure_before_it_is_the_harnesss(self):
        self.assertEqual(classify([step("a", "pass"), step("b", "skip", STOPPED)]), ([], ["b"]))

    def test_an_unknown_result_is_the_harnesss(self):
        self.assertEqual(classify([step("a", "error")]), ([], ["a"]))


if __name__ == "__main__":
    unittest.main()
