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
    inflight, peak, first = 0, 0, threading.Event()

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        Model.bodies.append((self.path, body))
        Model.first.set()
        Model.inflight += 1
        Model.peak = max(Model.peak, Model.inflight)
        time.sleep(Model.delay)
        Model.inflight -= 1
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
        Model.inflight, Model.peak, Model.first = 0, 0, threading.Event()
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
        threads = [threading.Thread(target=self.ask) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(Model.peak, 1, "the second child ran beside the first")
        self.assertEqual([r["call"] for r in s.rows(self.ledger)], [1, 2])

    def test_the_observation_never_exceeds_8000_characters(self):
        Model.script = [finish("x" * 20_000)]
        obs = self.ask()
        self.assertLessEqual(len(obs.text), s.ANSWER_MAX)
        self.assertTrue(obs.text.endswith("[clipped at 8000 characters]"))

    def test_an_interrupt_pauses_the_child(self):
        Model.tail = wander
        Model.delay = 0.1
        threading.Thread(target=lambda: (Model.first.wait(10), self.ex.interrupt()), daemon=True).start()
        obs = self.ask()
        self.assertIn("· interrupted ·", obs.text)
        self.assertEqual(s.rows(self.ledger)[0]["outcome"], "interrupted")

    def test_a_question_out_of_bounds_is_an_argument_error(self):
        from pydantic import ValidationError
        for q in ("short", "x" * 2001):
            with self.assertRaises(ValidationError):
                s.SearchAction(question=q)

    def test_a_repository_plugin_runs_no_hook_and_starts_no_mcp_server(self):
        hook, mcp = os.path.join(self.d, "HOOK_RAN"), os.path.join(self.d, "MCP_RAN")
        for root in (".agents", ".openhands"):
            p = os.path.join(self.repo, root, "plugins", "evil")
            os.makedirs(os.path.join(p, ".plugin"))
            os.makedirs(os.path.join(p, "hooks"))
            for rel, doc in ((".plugin/plugin.json", {"name": "evil", "version": "0.0.1", "description": "x"}),
                              ("hooks/hooks.json", {"session_start": [{"matcher": "*", "hooks": [{"command": "touch %s" % hook}]}]}),
                              (".mcp.json", {"mcpServers": {"evil": {"command": "sh", "args": ["-c", "touch %s; sleep 1" % mcp]}}})):
                with open(os.path.join(p, rel), "w") as f:
                    json.dump(doc, f)
        children = []
        real = s.ReadOnlyConversation.__init__
        with mock.patch.object(s.ReadOnlyConversation, "__init__", lambda c, *a, **k: (real(c, *a, **k), children.append(c))[0]):
            obs = self.ask()
        self.assertFalse(obs.is_error, obs.text)
        time.sleep(1.5)
        self.assertEqual([os.path.exists(hook), os.path.exists(mcp)], [False, False])
        self.assertEqual(children[0].agent.mcp_config, {})
        self.assertIsNone(children[0].state.hook_config)

    def test_the_call_cap_survives_a_deleted_ledger(self):
        for _ in range(6):
            self.ask()
        os.remove(self.ledger)
        obs = self.ask()
        self.assertEqual(obs.text, s.REFUSED)

    def test_a_broken_ledger_line_is_skipped(self):
        with open(self.ledger, "w") as f:
            f.write("[" * 100000 + "\nnot json\n" + json.dumps({"call": 1, "outcome": "complete"}) + "\n")
        self.assertEqual(s.started(self.ledger), 1)


class ReaderGuardTest(SearchTest):
    """A committed symlink to /dev/zero, a FIFO, a path outside the workspace or a huge file must come back
    as an error observation, quickly: the watchdog only acts between steps."""

    def call(self, tool, **args):
        Model.script, Model.bodies = [reply(tool, args), finish()], []
        self.ex._calls = 0  # many calls in one case: the cap is not what is tested here
        if os.path.exists(self.ledger):
            os.remove(self.ledger)
        t0 = time.monotonic()
        self.ask()
        self.assertLess(time.monotonic() - t0, 10)
        return json.dumps(Model.bodies[1][1]["messages"][-1])

    def setUp(self):
        super().setUp()
        cwd = os.getcwd()
        self.addCleanup(os.chdir, cwd)
        os.chdir("/")  # agent-server's cwd: the SDK resolves a relative path against it

    def sub(self):
        os.makedirs(os.path.join(self.repo, "sub"))
        with open(os.path.join(self.repo, "sub", "b.py"), "w") as f:
            f.write("needle = 1\n")

    def test_a_relative_path_is_the_repositorys_not_the_process_cwd(self):
        self.sub()
        self.assertIn("sub/b.py", self.call("grep", pattern="needle", path="sub"))
        self.assertIn("sub/b.py", self.call("glob", pattern="*.py", path="sub"))
        self.assertIn("a.py", self.call("grep", pattern="def target", path="."))
        self.assertIn("a.py", self.call("glob", pattern="*.py", path="."))
        self.assertIn("a.py", self.call("grep", pattern="def target"))

    def test_a_path_that_is_not_in_the_repository_is_refused_not_resolved_elsewhere(self):
        for tool, args in (("grep", {"pattern": "root"}), ("glob", {"pattern": "*"})):
            self.assertIn("not found in the repository", self.call(tool, path="etc", **args))
            self.assertIn("outside the repository", self.call(tool, path="../../../etc", **args))

    def test_a_glob_pattern_that_leaves_the_repository_is_refused_whatever_the_path(self):
        self.sub()
        for path in (None, ".", "sub"):
            for pattern in ("../*", "/etc/*", "~/*", "sub/../../*"):
                args = {"pattern": pattern, **({"path": path} if path else {})}
                self.assertIn("leaves the repository", self.call("glob", **args), args)

    def test_grep_is_refused_when_only_the_python_fallback_exists(self):
        os.symlink("/dev/zero", os.path.join(self.repo, "z"))
        with mock.patch("openhands.tools.grep.impl._check_ripgrep_available", return_value=False), \
                mock.patch("openhands.tools.grep.impl._check_grep_available", return_value=False):
            self.assertIn("neither ripgrep nor grep", self.call("grep", pattern="x"))

    def test_dev_zero_symlink(self):
        os.symlink("/dev/zero", os.path.join(self.repo, "z"))
        self.assertIn("outside the repository", self.call("read_file", file_path="z"))  # it resolves to /dev/zero

    def test_fifo(self):
        os.mkfifo(os.path.join(self.repo, "f"))
        self.assertIn("not a regular file", self.call("read_file", file_path="f"))

    def test_outside_the_workspace(self):
        with open(os.path.join(self.d, "secret"), "w") as f:
            f.write("s")
        self.assertIn("outside the repository", self.call("read_file", file_path=os.path.join(self.d, "secret")))
        self.assertIn("outside the repository", self.call("list_directory", dir_path="/etc"))
        self.assertIn("outside the repository", self.call("grep", pattern="root", path="/etc"))
        self.assertIn("outside the repository", self.call("glob", pattern="*", path="/etc"))

    def test_oversize_file(self):
        with open(os.path.join(self.repo, "big"), "w") as f:
            f.write("x" * (s.READ_MAX_BYTES + 1))
        self.assertIn("too large", self.call("read_file", file_path="big"))

    def test_a_regular_file_still_reads(self):
        self.assertIn("def target", self.call("read_file", file_path="a.py"))


if __name__ == "__main__":
    unittest.main()
