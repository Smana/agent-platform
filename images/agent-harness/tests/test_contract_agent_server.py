"""The tester's ends through a real agent-server (v3 feature 4, F1, offline proof 2): the model writes tests with
its terminal and commits them, its own push goes nowhere, and tester_end, with the real poll and
continue_conversation, checks the commit, signs and pushes it or sends the result back once in the same
conversation. The gateway is a stand-in that answers the agent with a script. Runs in the image:
docker build --target test.
"""
import http.server
import io
import json
import os
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_run  # noqa: E402
import test_agent_run_contract as arc  # noqa: E402

USAGE = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
COMMIT = "git -c user.name=t -c user.email=t@x -c core.hooksPath=/dev/null commit -qm tests"
RED_TEST = """printf '%s\\n' 'out="$(bash bin/greet ada)"; [ "$out" = "hello ada" ]' > t/test-name.sh"""
VACUOUS_TEST = "printf 'exit 0\\n' > t/test-vacuous.sh"


def tool_call(name, arguments):
    return json.dumps({"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE, "choices": [
        {"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None, "tool_calls": [
            {"id": "c%d" % time.monotonic_ns(), "type": "function", "function": {"name": name, "arguments": json.dumps(arguments)}}]}}]}).encode()


def shell(command):
    return ("terminal", {"command": command})


FINISH = ("finish", {"message": "done"})


class Gateway(http.server.BaseHTTPRequestHandler):
    """Each agent request gets the next scripted call, finish once the script runs out."""
    script, bodies = [], []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        Gateway.bodies.append(body)
        name, args = Gateway.script.pop(0) if Gateway.script else FINISH
        out = tool_call(name, args)
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass


class TesterThroughAgentServerTest(arc.Origin, unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gateway = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=cls.gateway.serve_forever, daemon=True).start()
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()
        # Set by hand: Origin's cleanup calls mock.patch.stopall, which would stop a class-level patch.
        cls.saved = {k: getattr(agent_run, k) for k in ("AGENT_SERVER", "POLL_INTERVAL_S")}
        agent_run.AGENT_SERVER, agent_run.POLL_INTERVAL_S = "http://127.0.0.1:%d" % port, 0.3
        cmd = [sys.executable] + agent_run.SERVER_CMD[1:]
        cmd[cmd.index("--port") + 1] = str(port)
        env = agent_run.server_env({k: v for k, v in os.environ.items() if k != "PYTHONPATH"})
        cls.server = subprocess.Popen(cmd, cwd=tempfile.mkdtemp(), env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        agent_run.wait_ready()

    @classmethod
    def tearDownClass(cls):
        cls.server.terminate()
        try:
            cls.server.wait(10)
        except subprocess.TimeoutExpired:
            cls.server.kill()
            cls.server.wait(10)
        cls.gateway.shutdown()
        cls.gateway.server_close()
        for k, v in cls.saved.items():
            setattr(agent_run, k, v)

    def setUp(self):
        super().setUp()
        trace = None
        try:
            import opentelemetry.trace  # noqa: F401
        except ImportError:
            trace = True
        self.assertIsNone(trace, "the image has opentelemetry; this suite runs there")
        agent_run.disable_push(self.env)

    def run_tester(self, script):
        """The tester's conversation to its first finish, then tester_end on the real poll and continuation."""
        Gateway.script, Gateway.bodies = list(script), []
        env = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:%d/v1" % self.gateway.server_port}
        request = agent_run.build_request(env, "write the failing tests for the greeting", "rules")
        del request["conversation_id"]
        self.cid = agent_run.http("POST", "/api/conversations", request)["id"]
        self.assertEqual(agent_run.poll(self.cid), 0)
        self.branch_after_conversation = git_branch(self.origin)
        notes = arc.Notes()
        with mock.patch("sys.stdout", new=io.StringIO()):
            end = agent_run.tester_end(self.env, self.cid, self.base, dict(os.environ), notes)
        self.notes = notes.finals
        return end

    def test_red_is_pushed_signed_with_one_conversation_and_the_models_push_goes_nowhere(self):
        end = self.run_tester([shell(f"{RED_TEST} && git add t && {COMMIT}; git push origin HEAD:agent/x 2>&1; echo pushed=$?"), FINISH])
        self.assertEqual(end, "contract_ready")
        self.assertEqual(self.branch_after_conversation, "", "nothing reached the origin before tester_end pushed")
        self.assertIn("disabled", json.dumps(Gateway.bodies[1]), "the model's own push failed on the disabled pushurl")
        self.assertEqual(git(self.origin, "log", "-1", "--format=%(trailers:key=Agent-Contract,valueonly)", "agent/x"), "run-t")
        self.assertEqual(git(self.origin, "rev-list", "--count", "main..agent/x"), "1")
        self.assertEqual(len(Gateway.bodies), 2, "one agent conversation, no continuation")
        self.assertEqual(self.notes, ["contract_ready: t/test-name.sh"])

    def test_a_vacuous_test_is_sent_back_in_the_same_conversation_once(self):
        end = self.run_tester([shell(f"{VACUOUS_TEST} && git add t && {COMMIT}"), FINISH,
                           shell(f"printf 'exit 0\\n' > t/test-vacuous2.sh && git add t && {COMMIT}"), FINISH])
        self.assertEqual(end, "contract_not_red")
        self.assertEqual(git_branch(self.origin), "")
        after = json.dumps(Gateway.bodies[-1]["messages"])
        self.assertIn("write the failing tests for the greeting", after, "the same conversation: its history is there")
        self.assertIn("CONTRACT-", after, "and the fenced result")
        self.assertIn("t/test-vacuous.sh", after)
        self.assertEqual(len([e for e in agent_run.http("GET", "/api/conversations/%s/events/search?limit=100" % self.cid)["items"]
                              if e.get("kind") == "MessageEvent" and e.get("source") == "user"]), 2, "the task, then one continuation")

    def test_outside_the_paths_continues_once_then_ends_outside(self):
        change = "printf '#!/usr/bin/env bash\\necho changed\\n' > bin/greet"
        end = self.run_tester([shell(f"{RED_TEST} && {change} && git add -A && {COMMIT}"), FINISH,
                           shell(f"git add -A && {COMMIT} --allow-empty"), FINISH])
        self.assertEqual(end, "contract_outside_tests")
        self.assertEqual(git_branch(self.origin), "")
        self.assertIn("CONTRACT-", json.dumps(Gateway.bodies[-1]["messages"]))

    def test_no_commit_is_not_testable_and_never_continues(self):
        end = self.run_tester([shell("echo nothing to do"), FINISH])
        self.assertEqual(end, "not_testable")
        self.assertEqual(len(Gateway.bodies), 2)
        self.assertEqual(git_branch(self.origin), "")


def git(cwd, *args):
    return arc.git(cwd, *args)


def git_branch(origin):
    return git(origin, "branch", "--list", "agent/x")


if __name__ == "__main__":
    unittest.main()
