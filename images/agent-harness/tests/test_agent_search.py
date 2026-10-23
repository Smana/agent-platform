"""search (v3 feature 5, offline proof 1): the child's tools, its policy, its bounds, the call cap, one
call at a time, and the observation's size. Each case runs a real child conversation against a scripted
model stand-in, in a git repository; the parent is a stub carrying what the executor reads from it.
"""
import http.server
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
from types import SimpleNamespace
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "site")
sys.path.insert(0, HERE)
import agent_search as s  # noqa: E402
from openhands.sdk import LLM  # noqa: E402
from openhands.sdk.llm.call_context import LLMCallContext  # noqa: E402
from openhands.sdk.security.confirmation_policy import NeverConfirm  # noqa: E402

USAGE = {"prompt_tokens": 1000, "completion_tokens": 10, "total_tokens": 1010}


def reply(name, arguments, usage=USAGE):
    return {"id": "r%d" % time.monotonic_ns(), "object": "chat.completion", "created": 0, "model": "m", "usage": usage,
            "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None,
                         "tool_calls": [{"id": "c%d" % time.monotonic_ns(), "type": "function",
                                         "function": {"name": name, "arguments": json.dumps(arguments)}}]}}]}


class Model(http.server.BaseHTTPRequestHandler):
    """Answers each call with the next scripted reply (a dict, or an int status), then `tail` (or tail(n))."""
    script, tail, bodies, delay = [], None, [], 0.0

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        Model.bodies.append((self.path, body))
        time.sleep(Model.delay)
        nxt = Model.script.pop(0) if Model.script else Model.tail
        if callable(nxt):
            nxt = nxt(len(Model.bodies))
        if isinstance(nxt, int):
            self.send_response(nxt)
            self.send_header("content-length", "0")
            self.end_headers()
            return
        out = json.dumps(nxt).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass


def wander(n, usage=USAGE):
    # A different thought each step: OpenHands' stuck detector catches identical cycles only.
    return reply("think", {"thought": "looking %d" % n}, usage)


def finish(text="Answer: in a.py.\nReferences: a.py:1\nNot found: nothing"):
    return reply("finish", {"message": text})


class SearchTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.model = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Model)
        threading.Thread(target=cls.model.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.model.shutdown()
        cls.model.server_close()

    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d)
        self.repo = os.path.join(self.d, "repo")
        os.makedirs(self.repo)
        env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t",
               "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}
        with open(os.path.join(self.repo, "a.py"), "w") as f:
            f.write("def target():\n    return 1\n")
        for args in (["init", "-q", "-b", "agent/x"], ["add", "a.py"], ["commit", "-q", "-m", "a"]):
            subprocess.run(["git", "-C", self.repo, *args], check=True, env=env, capture_output=True)
        self.sha = subprocess.run(["git", "-C", self.repo, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        self.ledger = os.path.join(self.d, "ledger.jsonl")
        Model.script, Model.tail, Model.bodies, Model.delay = [], finish(), [], 0.0
        llm = LLM(model="openai/agent-default", base_url="http://127.0.0.1:%d/v1" % self.model.server_port,
                  api_key="injected-by-identity-proxy", usage_id="agent", num_retries=0,
                  litellm_extra_body={"reasoning_effort": "high"})
        self.parent = SimpleNamespace(
            agent=SimpleNamespace(llm=llm), state=SimpleNamespace(workspace=SimpleNamespace(working_dir=self.repo), id=uuid.uuid4()),
            get_llm_call_context=lambda: LLMCallContext(), conversation_stats=SimpleNamespace(usage_to_metrics={}))
        self.ex = s.SearchExecutor(ledger=self.ledger)

    def ask(self, question="Where is target defined in this repository?"):
        return self.ex(s.SearchAction(question=question), self.parent)

    def test_the_childs_tools_are_the_four_readers_finish_and_think(self):
        self.ask()
        tools = sorted(t["function"]["name"] for t in Model.bodies[0][1]["tools"])
        self.assertEqual(tools, sorted(["grep", "glob", "read_file", "list_directory", "finish", "think"]))

    def test_no_mcp_tool_passes_the_filter(self):
        import re
        names = ["room_handoff", "room_post", "platform__room-broker_room_handoff", "terminal", "file_editor"]
        self.assertEqual([n for n in names if re.match(s.ONLY_READERS, n)], [])
        agent = s.child_agent(self.parent.agent.llm)
        self.assertEqual(agent.mcp_config, {})
        self.assertIsNone(agent.condenser)

    def test_the_child_never_asks_for_confirmation(self):
        seen = []
        real = s.LocalConversation.set_confirmation_policy
        with mock.patch.object(s.LocalConversation, "set_confirmation_policy",
                               lambda c, p: (seen.append(p), real(c, p))):
            self.ask()
        self.assertEqual([type(p) for p in seen], [NeverConfirm])

    def test_an_answer_comes_back_with_its_header_and_the_ledger_row(self):
        Model.script = [reply("grep", {"pattern": "def target"}), finish()]
        obs = self.ask()
        self.assertFalse(obs.is_error)
        self.assertTrue(obs.text.startswith("search 1/6 · complete · 2 steps · 2k tokens · read at %s" % self.sha[:8]), obs.text)
        self.assertIn("References: a.py:1", obs.text)
        row = s.rows(self.ledger)[0]
        self.assertEqual((row["call"], row["outcome"], row["toolCalls"], row["head"]), (1, "complete", 2, self.sha))
        self.assertEqual(row["tokens"]["input"], 2000)
        self.assertIn("search:1", self.parent.conversation_stats.usage_to_metrics)

    def test_the_child_runs_at_low_effort_on_the_runs_model_and_base(self):
        self.ask()
        path, body = Model.bodies[0]
        self.assertEqual(path, "/v1/chat/completions")
        self.assertEqual(body["model"], "agent-default")
        self.assertEqual(body.get("reasoning_effort"), "low")
        self.assertIn("Question: Where is target defined", json.dumps(body["messages"]))

    def test_a_write_tool_the_child_asks_for_is_unknown_and_changes_nothing(self):
        Model.script = [reply("terminal", {"command": "rm a.py"}), reply("file_editor", {"command": "create", "path": "b", "file_text": "x"}),
                        reply("room_handoff", {"summary": "x"}), finish()]
        obs = self.ask()
        self.assertFalse(obs.is_error, obs.text)
        self.assertEqual(sorted(os.listdir(self.repo)), [".git", "a.py"])
        self.assertIn("not found", json.dumps(Model.bodies[1][1]["messages"]).lower())

    def test_the_41st_iteration_ends_budget_with_the_partial_answer(self):
        Model.tail = wander
        obs = self.ask()
        self.assertTrue(obs.is_error)
        self.assertIn("· budget · 40 steps", obs.text)
        self.assertIn("stopped: 40 iterations.", obs.text)
        self.assertEqual(s.rows(self.ledger)[0]["outcome"], "budget")
        self.assertEqual(len(Model.bodies), 40)

    def test_the_token_bound_ends_budget(self):
        Model.tail = lambda n: wander(n, {"prompt_tokens": 200_000, "completion_tokens": 1, "total_tokens": 200_001})
        with mock.patch.object(s, "WATCH_S", 0.05):
            Model.delay = 0.2
            obs = self.ask()
        self.assertIn("stopped: 300000 tokens.", obs.text)
        self.assertLess(len(Model.bodies), 5)

    def test_the_clock_bound_ends_budget(self):
        Model.tail = wander
        Model.delay = 0.2
        with mock.patch.object(s, "WATCH_S", 0.05), mock.patch.object(s, "MAX_SECONDS", 0.5):
            obs = self.ask()
        self.assertIn("stopped: 0 s.", obs.text)  # %d of 0.5 s
        self.assertEqual(s.rows(self.ledger)[0]["outcome"], "budget")

    def test_a_model_that_fails_returns_an_error_observation(self):
        Model.script, Model.tail = [500], 500
        obs = self.ask()
        self.assertTrue(obs.is_error)
        self.assertIn("· error ·", obs.text)
        self.assertEqual(s.rows(self.ledger)[0]["outcome"], "error")

    def test_the_7th_call_is_refused_from_the_ledger(self):
        with open(self.ledger, "w") as f:
            for i in range(6):
                f.write(json.dumps({"call": i + 1, "outcome": ["complete", "budget", "error"][i % 3]}) + "\n")
            f.write(json.dumps({"call": 7, "outcome": "refused"}) + "\n")
        obs = self.ask()
        self.assertTrue(obs.is_error)
        self.assertEqual(obs.text, s.REFUSED)
        self.assertEqual(Model.bodies, [])
        self.assertEqual(s.rows(self.ledger)[-1]["outcome"], "refused")

    def test_two_concurrent_calls_run_one_after_the_other(self):
        Model.delay = 0.3
        spans = []

        def one():
            t0 = time.monotonic()
            self.ask()
            spans.append((t0, time.monotonic()))

        threads = [threading.Thread(target=one) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        (a0, a1), (b0, b1) = sorted(spans, key=lambda x: x[1])
        self.assertGreaterEqual(b1 - a1, 0.25, "the second child ran beside the first")
        self.assertEqual([r["call"] for r in s.rows(self.ledger)], [1, 2])

    def test_the_observation_never_exceeds_8000_characters(self):
        Model.script = [finish("x" * 20_000)]
        obs = self.ask()
        self.assertLessEqual(len(obs.text), s.ANSWER_MAX)
        self.assertTrue(obs.text.endswith("[clipped at 8000 characters]"))

    def test_an_interrupt_pauses_the_child(self):
        Model.tail = wander
        Model.delay = 0.1
        threading.Timer(0.5, self.ex.interrupt).start()
        obs = self.ask()
        self.assertIn("· interrupted ·", obs.text)
        self.assertEqual(s.rows(self.ledger)[0]["outcome"], "interrupted")

    def test_a_question_out_of_bounds_is_an_argument_error(self):
        from pydantic import ValidationError
        for q in ("short", "x" * 2001):
            with self.assertRaises(ValidationError):
                s.SearchAction(question=q)


if __name__ == "__main__":
    unittest.main()
