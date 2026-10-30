"""The judge round's triage of a not_yet (harness roles HR5): a not_yet whose only objection is the guard's
pending check spends no continuation; the harness waits for the checks within the box the continuation
would have had, then asks the judge again.
"""
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402
import loop_policy as lp  # noqa: E402

POLICY = lp.Policy(stall_steps=80, continuations=2, judge_model="tier-light")


def offered(pending=(), failing=(), unpushed=False):
    o = lp.Offered(["diff", *pending, *failing])
    o.pending, o.failing, o.unpushed = tuple(pending), tuple(failing), unpushed
    return o


def runs(*statuses):
    """check_runs' answer: one run per status, named check:<i>."""
    return [{"name": str(i), "status": "completed" if s in ("success", "failure") else "in_progress",
             "conclusion": s if s in ("success", "failure") else "", "title": ""} for i, s in enumerate(statuses)]


class Steps:
    steps = 0

    def __call__(self):
        pass


class Clock:
    def __init__(self):
        self.now, self.slept = 0.0, []

    def __call__(self):
        return self.now

    def sleep(self, s):
        self.slept.append(s)
        self.now += s


class AwaitChecksTest(unittest.TestCase):
    """await_checks: the head's check runs every PENDING_POLL_S until none is pending or the box ends."""

    def wait(self, answers, seconds=15 * 60):
        self.clock, self.evidence_calls = Clock(), 0
        answers = iter(answers)

        def ev(env, task):
            self.evidence_calls += 1
            return "rebuilt", offered()

        with mock.patch("agent_run.check_runs", side_effect=lambda env, sha: next(answers)), \
                mock.patch("agent_run.head", return_value="h"), mock.patch("agent_run.evidence", side_effect=ev):
            return agent_run.await_checks({}, "task", seconds, clock=self.clock, sleep=self.clock.sleep)

    def test_checks_that_complete_rebuild_the_evidence_once(self):
        self.assertEqual(self.wait([runs("pending"), runs("pending"), runs("success")])[0], "rebuilt")
        self.assertEqual(self.clock.slept, [agent_run.PENDING_POLL_S] * 2)
        self.assertEqual(self.evidence_calls, 1)

    def test_a_box_that_ends_first_is_none_and_reads_no_evidence(self):
        self.assertIsNone(self.wait([runs("pending")] * 1000, seconds=5 * 60))
        self.assertLessEqual(self.clock.now, 5 * 60)
        self.assertEqual(self.evidence_calls, 0)

    def test_the_box_is_fixed_once_so_the_wait_ends(self):
        # R9: no deadline still gives the wait an end; the box is not recomputed per poll.
        self.assertIsNone(self.wait([runs("pending")] * 1000, seconds=agent_run.CONTINUATION_S))
        self.assertLessEqual(self.clock.now, agent_run.CONTINUATION_S)
        self.assertEqual(len(self.clock.slept), agent_run.CONTINUATION_S // agent_run.PENDING_POLL_S)

    def test_github_not_answering_stops_the_wait_and_lets_the_guard_read_it(self):
        self.assertEqual(self.wait([None])[0], "rebuilt")
        self.assertEqual(self.clock.slept, [])


class JudgeTriageTest(unittest.TestCase):
    """drive()'s judge round over scripted evidence, check runs, judge answers and a fake clock."""

    def round(self, evidence, answers, checks=(), deadline=45 * 60, state=None):
        self.messages, self.clock = [], Clock()
        self.state = state or lp.State(start="s", committed=True)
        polls, checks = iter([0, 0, 0]), iter(checks)
        with mock.patch("agent_run.poll", side_effect=lambda cid, on_tick=None: next(polls)), \
                mock.patch("agent_run.http", return_value={}), \
                mock.patch("agent_run.evidence", side_effect=[("e", o) for o in evidence]), \
                mock.patch("agent_run.check_runs", side_effect=lambda env, sha: next(checks)), \
                mock.patch("agent_run.ask_judge", side_effect=[self.answer(a) for a in answers]), \
                mock.patch("agent_run.commits_since", return_value=1), mock.patch("agent_run.head", return_value="h"), \
                mock.patch("agent_run.continue_conversation", side_effect=lambda cid, text: self.messages.append(text)), \
                mock.patch("sys.stdout", new=io.StringIO()) as out:
            result = agent_run.drive({}, "cid", Steps(), POLICY, self.state, "task",
                                     os.path.join(tempfile.mkdtemp(), "loop.json"),
                                     deadline=deadline, clock=self.clock, sleep=self.clock.sleep)
        self.out = out.getvalue()
        return result

    @staticmethod
    def answer(kind):
        return json.dumps({"verdict": kind, "reason": "r", "evidence": ["diff"]}), "glm-5.3-flash"

    def test_a_pending_only_not_yet_waits_and_spends_no_continuation(self):
        """HR5: the judge said met, the guard found only a pending check: the harness waits for the checks
        and asks again; no message goes to the agent and no continuation is counted."""
        result = self.round([offered(pending=["check:0"]), offered()], ["met", "met"],
                            checks=[runs("pending"), runs("success")])
        self.assertEqual(result, (0, None, ""))
        self.assertEqual(self.messages, [])
        self.assertEqual(self.state.sent, 0)
        self.assertIn("agent-run judge 2: met", self.out)

    def test_the_second_answer_is_guarded_and_decided_as_usual(self):
        result = self.round([offered(pending=["check:0"]), offered(failing=["check:0"]), offered()],
                            ["met", "met", "met"], checks=[runs("failure")])
        self.assertEqual(result, (0, None, ""))
        [message] = self.messages
        self.assertIn("check:0", message)
        self.assertEqual(self.state.sent, 1)

    def test_a_failing_check_still_continues(self):
        self.round([offered(failing=["check:0"]), offered()], ["met", "met"])
        self.assertEqual(len(self.messages), 1)
        self.assertEqual(self.state.sent, 1)

    def test_a_pending_check_beside_another_objection_still_continues(self):
        self.round([offered(pending=["check:0"], failing=["check:1"]), offered()], ["met", "met"])
        self.assertEqual(len(self.messages), 1)
        self.round([offered(pending=["check:0"], unpushed=True), offered()], ["met", "met"])
        self.assertEqual(len(self.messages), 1)

    def test_the_judges_own_not_yet_still_continues(self):
        self.round([offered(pending=["check:0"]), offered()], ["not_yet", "met"])
        self.assertEqual(len(self.messages), 1)
        self.assertEqual(self.state.sent, 1)

    def test_checks_pending_past_the_box_end_not_met(self):
        code, reason, note = self.round([offered(pending=["check:0"])], ["met"], checks=[runs("pending")] * 1000)
        self.assertEqual((code, reason, note), (1, "not_met", "checks still pending after 15 minutes"))
        self.assertEqual(self.messages, [])
        self.assertEqual(self.state.sent, 0)
        self.assertLessEqual(self.clock.now, agent_run.CONTINUATION_S)

    def test_the_wait_is_boxed_by_the_runs_deadline(self):
        _, _, note = self.round([offered(pending=["check:0"])], ["met"], checks=[runs("pending")] * 1000, deadline=10 * 60)
        self.assertEqual(note, "checks still pending after 7 minutes")
        self.assertLessEqual(self.clock.now, 10 * 60 - agent_run.CONTINUATION_MARGIN_S)

    def test_no_deadline_still_ends_the_wait(self):
        _, _, note = self.round([offered(pending=["check:0"])], ["met"], checks=[runs("pending")] * 1000, deadline=None)
        self.assertEqual(note, "checks still pending after 15 minutes")

    def test_too_little_time_left_ends_as_too_late_without_waiting(self):
        code, reason, note = self.round([offered(pending=["check:0"])], ["met"], deadline=agent_run.CONTINUATION_MARGIN_S + 60)
        self.assertEqual((code, reason), (1, "not_met"))
        self.assertTrue(note.endswith("too little of the run's time was left for a continuation"), note)
        self.assertEqual((self.messages, self.clock.slept, self.state.sent), ([], [], 0))


if __name__ == "__main__":
    unittest.main()
