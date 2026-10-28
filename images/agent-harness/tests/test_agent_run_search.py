"""agent-run's half of search as a tool (v3 feature 5, offline proof 3): the switch, the start request,
the rule, the ledger's steps in the stall count, and the summary line. Needs the openhands SDK: it runs
in the image (docker build --target test).
"""
import builtins
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

GOLDEN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "golden", "start-request.json")
ENV = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:4000/v1", "MCP_URL": "http://127.0.0.1:4000/mcp",
       "CONVERSATION_ID": "0b3c6f0e-5d1a-4b8e-9f41-2a7c3e9d8b10", "ROLE": "implementer"}


def request(env, search):
    return json.dumps(agent_run.build_request(env, "fix the link", "rules text", None, search), indent=1, sort_keys=True) + "\n"


class SwitchTest(unittest.TestCase):
    def on(self, env):
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            return agent_run.search_on(env), err.getvalue()

    def test_without_the_switch_or_for_a_reviewer_the_request_is_todays(self):
        with open(GOLDEN) as f:
            golden = f.read()
        for env in (ENV, {**ENV, "AGENT_SEARCH": "1", "ROLE": "reviewer"}, {**ENV, "AGENT_SEARCH": "0"}):
            on, _ = self.on(env)
            self.assertFalse(on, env)
            self.assertEqual(request(env, on), golden, env)
        self.assertEqual(agent_run.search_rules(False), "")

    def test_the_rule_makes_search_the_first_move_for_lookups_and_is_bounded(self):
        rule = agent_run.search_rules(True)
        for text in ("call `search` FIRST", "before grep, find or cat", "which file, runbook or doc", "Skip it when the task needs no lookup"):
            self.assertIn(text, rule)
        for on in (True, False):  # present only when search is on
            body = agent_run.build_request(ENV, "t", "r" + agent_run.search_rules(on), None, on)
            self.assertEqual("FIRST" in body["agent_launch_additions"]["system_message_suffix_append"], on)

    def test_a_reviewer_is_told_why(self):
        self.assertEqual(self.on({**ENV, "AGENT_SEARCH": "1", "ROLE": "reviewer"}), (False, "agent-run: search off: the reviewer role has none\n"))

    def test_an_implementer_gets_the_tool_its_module_and_the_rule(self):
        on, _ = self.on({**ENV, "AGENT_SEARCH": "1"})
        self.assertTrue(on)
        body = agent_run.build_request(ENV, "t", "r" + agent_run.search_rules(on), None, on)
        self.assertEqual([t["name"] for t in body["agent"]["tools"]], ["terminal", "file_editor", "task_tracker", "search"])
        self.assertEqual(body["tool_module_qualnames"], {"search": "agent_search"})
        self.assertTrue(body["agent_launch_additions"]["system_message_suffix_append"].endswith(
            "\nSearch:\n- " + agent_run.SEARCH_RULE + "\n"))
        from openhands.sdk.conversation.request import StartConversationRequest
        StartConversationRequest.model_validate(body)  # agent-server's own model accepts it

    def test_a_module_that_does_not_import_is_search_off(self):
        real = builtins.__import__

        def refuse(name, *args, **kwargs):
            if name == "agent_search":
                raise ImportError("No module named 'openhands.tools.grep'")
            return real(name, *args, **kwargs)

        sys.modules.pop("agent_search", None)
        with mock.patch("builtins.__import__", refuse):
            on, err = self.on({**ENV, "AGENT_SEARCH": "1"})
        self.assertFalse(on)
        self.assertIn("agent-run: search off: No module named", err)
        self.assertNotIn('"search"', request(ENV, on))


class LedgerTest(unittest.TestCase):
    def setUp(self):
        self.ledger = os.path.join(tempfile.mkdtemp(), "search.jsonl")

    def write(self, *rows):
        with open(self.ledger, "a") as f:
            for r in rows:
                f.write((r if isinstance(r, str) else json.dumps(r)) + "\n")

    def tick(self, log):
        with mock.patch("agent_run.http", return_value={"items": [], "next_page_id": None}), \
                mock.patch("sys.stdout", new=io.StringIO()):
            log()

    def test_the_childs_tool_calls_join_the_step_count(self):
        log = agent_run.StepLog("cid", ledger=self.ledger)
        self.write({"outcome": "complete", "toolCalls": 23, "tokens": {"input": 180000, "output": 7000}},
                   {"outcome": "refused"}, "not json", {"outcome": "complete", "toolCalls": "lots"}, {"outcome": "made-up", "toolCalls": 99})
        self.tick(log)
        self.assertEqual(log.steps, 23)
        off = agent_run.StepLog("cid")
        self.tick(off)
        self.assertEqual(off.steps, 0, "search off: the ledger is not read")

    def test_a_search_fires_the_stall_rule_at_stall_steps(self):
        log = agent_run.StepLog("cid", ledger=self.ledger)
        self.write({"outcome": "budget", "toolCalls": 40})
        policy = lp.Policy(stall_steps=40, continuations=2, judge_model=None)
        with mock.patch("agent_run.commits_since", return_value=0), \
                mock.patch("agent_run.http", return_value={"items": [], "next_page_id": None}), \
                mock.patch("sys.stdout", new=io.StringIO()):
            tick = agent_run.stall_watch({}, log, policy, lp.State(start="s"), os.path.join(tempfile.mkdtemp(), "loop.json"))
            with self.assertRaises(agent_run.Stalled):
                tick()

    def test_the_summary_line(self):
        self.write({"outcome": "complete", "toolCalls": 23, "tokens": {"input": 180000, "output": 7000, "cacheRead": 90000}},
                   {"outcome": "budget", "toolCalls": 40, "tokens": {"input": 300000, "output": 2000}},
                   {"outcome": "refused"})
        self.assertEqual(agent_run.search_summary(self.ledger),
                         "agent-run search: 2 calls, 63 steps, 489000 tokens, outcomes budget=1 complete=1 refused=1")
        self.assertEqual(agent_run.search_summary(self.ledger + ".none"),
                         "agent-run search: 0 calls, 0 steps, 0 tokens, outcomes none")


if __name__ == "__main__":
    unittest.main()
