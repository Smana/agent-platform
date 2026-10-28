"""The loop policy through a real agent-server 1.49.6 (loops design L2, L3): a continuation runs the
same conversation again with its history, and a stall interrupts a running one. The gateway is a
stand-in that answers the agent with a script and the judge with its verdicts.
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
import agent_run  # noqa: E402
import loop_policy as lp  # noqa: E402

USAGE = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}


def tool_call(name, arguments):
    return json.dumps({"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE, "choices": [
        {"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None, "tool_calls": [
            {"id": "c%d" % time.monotonic_ns(), "type": "function", "function": {"name": name, "arguments": json.dumps(arguments)}}]}}]}).encode()


class Gateway(http.server.BaseHTTPRequestHandler):
    """The agent's calls get `agent` (finish, or think forever); the judge's get the next verdict."""
    agent, verdicts, agent_bodies, judge_bodies = "finish", [], [], []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        if body["messages"][0].get("content") == lp.JUDGE_SYSTEM:
            Gateway.judge_bodies.append((self.path, body))
            out = json.dumps({"model": "glm-5.3-flash", "usage": USAGE, "choices": [{"index": 0, "finish_reason": "stop",
                              "message": {"role": "assistant", "content": Gateway.verdicts.pop(0)}}]}).encode()
        else:
            Gateway.agent_bodies.append(body)
            # A different thought each step, as the wandering run's greps were: OpenHands' stuck
            # detector catches identical cycles only.
            thought = "grep %d" % len(Gateway.agent_bodies)
            out = tool_call("finish", {"message": "done"}) if Gateway.agent == "finish" else tool_call("think", {"thought": thought})
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass


def verdict(kind, reason):
    return json.dumps({"verdict": kind, "reason": reason, "evidence": ["diff"]})


class LoopThroughAgentServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gateway = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=cls.gateway.serve_forever, daemon=True).start()
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()
        cls.patches = [mock.patch.object(agent_run, "AGENT_SERVER", "http://127.0.0.1:%d" % port),
                       mock.patch.object(agent_run, "REPO_DIR", tempfile.mkdtemp()),
                       mock.patch.object(agent_run, "POLL_INTERVAL_S", 0.3)]
        for p in cls.patches:
            p.start()
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
        for p in cls.patches:
            p.stop()

    def start(self, agent, verdicts):
        Gateway.agent, Gateway.verdicts, Gateway.agent_bodies, Gateway.judge_bodies = agent, list(verdicts), [], []
        env = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:%d/v1" % self.gateway.server_port}
        request = agent_run.build_request(env, "fix the link", "rules")
        del request["conversation_id"]
        return env, agent_run.http("POST", "/api/conversations", request)["id"]

    def drive(self, env, cid, policy, commits):
        state = lp.State(start="s")
        steps = agent_run.StepLog(cid)
        with mock.patch("agent_run.evidence", return_value=("evidence", {"diff"})), \
                mock.patch("agent_run.commits_since", return_value=commits), mock.patch("agent_run.head", return_value="h"), \
                mock.patch("sys.stdout", new=io.StringIO()) as out, mock.patch("sys.stderr", new=io.StringIO()):
            result = agent_run.drive(env, cid, steps, policy, state, "task", os.path.join(tempfile.mkdtemp(), "loop.json"))
        return result, state, out.getvalue()

    def events(self, cid):
        return agent_run.http("GET", "/api/conversations/%s/events/search?limit=100" % cid)["items"]

    def test_not_yet_runs_the_same_conversation_again_with_its_history(self):
        env, cid = self.start("finish", [verdict("not_yet", "the lint check fails"), verdict("met", "green")])
        result, state, out = self.drive(env, cid, lp.Policy(80, 2, "tier-light"), commits=1)
        self.assertEqual(result, (0, None, ""), out)
        self.assertEqual(state.sent, 1)
        self.assertEqual([p for p, _ in Gateway.judge_bodies], ["/judge/v1/chat/completions"] * 2)
        self.assertEqual(len(Gateway.agent_bodies), 2, "one agent turn per run")
        # The second turn's request carries the first turn and the continuation: one conversation.
        second = json.dumps(Gateway.agent_bodies[1]["messages"])
        self.assertIn("fix the link", second)
        self.assertIn("the lint check fails", second)
        users = [e for e in self.events(cid) if e.get("kind") == "MessageEvent" and e.get("source") == "user"]
        self.assertEqual(len(users), 2, "the task, then one continuation")
        self.assertEqual(agent_run.http("GET", "/api/conversations/" + cid)["execution_status"], "finished")

    def test_a_stall_interrupts_a_running_conversation(self):
        env, cid = self.start("think", [])
        result, _, out = self.drive(env, cid, lp.Policy(3, 2, "tier-light"), commits=0)
        self.assertEqual(result, (1, "stalled", "no commit after 3 steps"), out)
        self.assertEqual(Gateway.judge_bodies, [], "a stall ends the run with no judge")
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            status = agent_run.http("GET", "/api/conversations/" + cid)["execution_status"]
            if status != "running":
                break
            time.sleep(0.2)
        self.assertEqual(status, "paused")


if __name__ == "__main__":
    unittest.main()
