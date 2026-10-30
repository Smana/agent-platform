"""The scoper's end (harness roles HR2, harness side): the bounds read from the brief's first line, one nudge
at nudgeAt tool calls, the stop at maxToolCalls with end reason budget_calls, and a handoff ending the count.
"""
import io
import os
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402

MARKER = "<!-- agent-factory scoper nudgeAt=3 maxToolCalls=5 -->\nYou are the scoper for agent factory task x in o/r.\n"
SCOPER = {"ROLE": "triager"}


class BoundsTest(unittest.TestCase):
    def bounds(self, task, env=SCOPER):
        with mock.patch("sys.stderr", new=io.StringIO()):
            return agent_run.scoper_bounds(env, task)

    def test_the_bounds_are_read_from_the_marker_on_line_one(self):
        self.assertEqual(self.bounds(MARKER), (3, 5))
        self.assertEqual(self.bounds("<!-- agent-factory scoper nudgeAt=25 maxToolCalls=40 -->\n"), (25, 40))

    def test_any_other_run_has_no_bound(self):
        self.assertIsNone(self.bounds("You are the implementer..."))
        self.assertIsNone(self.bounds(MARKER, {"ROLE": "implementer"}))
        self.assertIsNone(self.bounds(MARKER, {}))

    def test_a_marker_past_offset_0_is_never_read(self):
        # The task text carries the untrusted issue: a marker anywhere but line 1 is the issue's, not the factory's.
        self.assertIsNone(self.bounds("Resumed.\n" + MARKER))
        self.assertIsNone(self.bounds(" " + MARKER))
        self.assertIsNone(self.bounds("You are the scoper.\n<!-- agent-factory scoper nudgeAt=1 maxToolCalls=2 -->\n"))

    def test_bounds_out_of_range_leave_the_run_unbounded(self):
        for nudge, limit in ((0, 5), (5, 5), (6, 5), (3, 201), (0, 0)):
            with self.subTest(nudge=nudge, limit=limit):
                self.assertIsNone(self.bounds("<!-- agent-factory scoper nudgeAt=%d maxToolCalls=%d -->\n" % (nudge, limit)))
        self.assertEqual(self.bounds("<!-- agent-factory scoper nudgeAt=1 maxToolCalls=200 -->\n"), (1, 200))


class Conversation:
    """agent-server over http: one action more per events read, the status it is given, the POSTs it got. A tool
    "name:ok", ":error" or ":agent_error" comes with its result; ":later" gets an ok result one read later."""

    def __init__(self, tools, finished_after=50):  # a finish at last: a regression fails, never hangs
        self.tools, self.finished_after = list(tools), finished_after
        self.shown, self.posts, self.interrupted, self.status_reads = 0, [], 0, 0

    def http(self, method, path, body=None, timeout=30):
        if path.endswith("/interrupt"):
            self.interrupted += 1
            return {}
        if method == "POST":
            self.posts.append(body)
            return {}
        if "/events/search" in path:
            self.shown = min(self.shown + 1, len(self.tools))
            return {"items": self.events(), "next_page_id": None}
        self.status_reads += 1
        if self.interrupted:
            return {"execution_status": "paused"}
        done = self.finished_after is not None and self.status_reads > self.finished_after
        return {"execution_status": "finished" if done else "running"}

    def events(self):
        items = []
        for i, t in enumerate(self.tools[:self.shown]):
            tool, _, result = t.partition(":")
            items.append({"id": "a%d" % i, "kind": "ActionEvent", "tool_name": tool, "tool_call_id": "c%d" % i,
                          "action": {"command": "cat f%d" % i}})
            if result == "later" and i < self.shown - 1:
                result = "ok"
            if result in ("ok", "error"):
                items.append({"id": "o%d" % i, "kind": "ObservationEvent", "tool_call_id": "c%d" % i,
                              "observation": {"content": [{"type": "text", "text": "x"}], "is_error": result == "error"}})
            elif result == "agent_error":
                items.append({"id": "o%d" % i, "kind": "AgentErrorEvent", "tool_call_id": "c%d" % i, "error": "bad args"})
        return items

    def nudges(self):
        return [b for b in self.posts if "Hand off what you have now" in b["content"][0]["text"]]


class CountTest(unittest.TestCase):
    def run_scoper(self, conv, bounds=(3, 5)):
        steps = agent_run.StepLog("cid", scoper=bounds)
        with mock.patch("agent_run.http", side_effect=conv.http), mock.patch("agent_run.time.sleep"), \
                mock.patch("sys.stdout", new=io.StringIO()), mock.patch("sys.stderr", new=io.StringIO()):
            return agent_run.scoper_poll("cid", steps), steps

    def test_the_scoper_is_nudged_once_then_stopped(self):
        conv = Conversation(["terminal"] * 8)
        (code, end, note), steps = self.run_scoper(conv)
        self.assertEqual(len(conv.nudges()), 1)
        nudge = conv.nudges()[0]
        self.assertIs(nudge["run"], False)  # the search nudge's path: read at the agent's next step
        self.assertTrue(nudge["content"][0]["text"].startswith("You have made 3 tool calls. Hand off what you have now"))
        self.assertEqual(conv.interrupted, 1)
        self.assertEqual((code, end), (1, "budget_calls"))
        self.assertEqual(note, "the scoper stopped after 5 tool calls without a handoff")
        self.assertEqual(steps.own, 5)

    def test_a_handoff_ends_the_count(self):
        conv = Conversation(["terminal", "terminal", "factory_room_handoff:ok", "terminal", "terminal", "terminal"],
                            finished_after=7)
        (code, end, note), _ = self.run_scoper(conv)
        self.assertEqual((code, end, note), (0, None, ""))
        self.assertEqual(conv.nudges(), [])
        self.assertEqual(conv.interrupted, 0)

    def test_a_handoff_as_the_last_allowed_call_is_not_stopped(self):
        # In flight at the limit, then it succeeds: no stop mid-flight, and the count is over.
        conv = Conversation(["terminal"] * 4 + ["room_handoff:later", "terminal", "terminal"], finished_after=8)
        (code, end, _), steps = self.run_scoper(conv)
        self.assertEqual((code, end), (0, None))
        self.assertEqual(conv.interrupted, 0)
        self.assertTrue(steps.handed_off)

    def test_a_failed_handoff_is_no_handoff(self):
        for result in ("error", "agent_error"):
            with self.subTest(result=result):
                conv = Conversation(["terminal", "room_handoff:" + result] + ["terminal"] * 6)
                (code, end, note), steps = self.run_scoper(conv)
                self.assertEqual((code, end), (1, "budget_calls"))
                self.assertEqual(note, "the scoper stopped after 5 tool calls without a handoff")
                self.assertEqual(len(conv.nudges()), 1)
                self.assertFalse(steps.handed_off)

    def test_a_resumed_pod_past_nudge_at_sends_no_second_nudge(self):
        conv = Conversation(["terminal"] * 8)
        conv.shown = 3  # the first pod's three calls, re-read on the first tick
        steps = agent_run.StepLog("cid", scoper=(3, 5), resumed=True)
        with mock.patch("agent_run.http", side_effect=conv.http), mock.patch("agent_run.time.sleep"), \
                mock.patch("sys.stdout", new=io.StringIO()), mock.patch("sys.stderr", new=io.StringIO()):
            self.assertEqual(agent_run.scoper_poll("cid", steps)[1], "budget_calls")
        self.assertEqual(conv.nudges(), [])

    def test_a_finish_read_on_the_poll_that_crosses_the_limit_is_a_finish(self):
        conv = Conversation(["terminal"] * 5, finished_after=4)
        (code, end, _), _ = self.run_scoper(conv)
        self.assertEqual((code, end), (0, None))
        self.assertEqual(conv.interrupted, 0)

    def test_no_bound_is_todays_run(self):
        conv = Conversation(["terminal"] * 8, finished_after=8)
        (code, end, _), steps = self.run_scoper(conv, bounds=None)
        self.assertEqual((code, end), (0, None))
        self.assertEqual((conv.posts, conv.interrupted, steps.own), ([], 0, 8))

    def test_a_nudge_that_fails_never_fails_the_run(self):
        conv = Conversation(["terminal"] * 8)
        inner = conv.http

        def http(method, path, body=None, timeout=30):
            if method == "POST" and path.endswith("/events"):
                raise OSError("refused")
            return inner(method, path, body, timeout)
        conv.http = http
        (code, end, _), _ = self.run_scoper(conv)
        self.assertEqual((code, end), (1, "budget_calls"))


class MainTest(unittest.TestCase):
    """main()'s wiring: the bounds reach the step log, and a budget_calls end reaches the room note and the bridge."""

    def main(self, role, text, result):
        with tempfile.TemporaryDirectory() as d:
            task = os.path.join(d, "task.md")
            with open(task, "w") as f:
                f.write(text)
            env = {"TASK_FILE": task, "RULES_FILE": task, "ROLE": role, "BRIDGE_URL": "http://b", "MCP_URL": "http://m"}
            with mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch("agent_run.subprocess.Popen"), mock.patch("agent_run.wait_ready"), \
                    mock.patch("agent_run.persisted", return_value=False), mock.patch("agent_run.clone", return_value=False), \
                    mock.patch("agent_run.build_request", return_value={}), \
                    mock.patch("agent_run.http", return_value={"id": "c1"}), \
                    mock.patch("agent_run.scoper_poll", return_value=result) as poll, \
                    mock.patch("agent_run.StepLog") as steps, mock.patch("agent_run.ProgressNotes") as notes, \
                    mock.patch("agent_run.final_read") as final_read, \
                    mock.patch("agent_run.stop", return_value="stopped"), mock.patch("agent_run.revoke"), \
                    mock.patch("sys.stdout", new=io.StringIO()), mock.patch("sys.stderr", new=io.StringIO()):
                code = agent_run.main()
        return code, steps.call_args.args[-2:], poll, notes.return_value, final_read

    def test_a_stopped_scoper_ends_budget_calls_with_one_room_note(self):
        stop = (1, "budget_calls", "the scoper stopped after 5 tool calls without a handoff")
        code, bounds, poll, notes, final_read = self.main("triager", MARKER, stop)
        self.assertEqual((code, bounds), (1, ((3, 5), False)))
        poll.assert_called_once()
        notes.final.assert_called_once_with("the run ended budget_calls: the scoper stopped after 5 tool calls without a handoff")
        final_read.assert_called_once_with(mock.ANY, end="budget_calls")

    def test_another_run_is_todays(self):
        code, bounds, _, notes, final_read = self.main("implementer", MARKER, (0, None, ""))
        self.assertEqual((code, bounds), (0, (None, False)))
        notes.final.assert_not_called()
        final_read.assert_called_once_with(mock.ANY, end=None)


if __name__ == "__main__":
    unittest.main()
