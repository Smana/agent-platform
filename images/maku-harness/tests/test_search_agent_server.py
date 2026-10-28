"""search as a tool through a real agent-server 1.56.0 (v3 feature 5, offline proof 2): an implementer's
`search` call runs a child on the same /v1 with the run's model; a write tool the child names is
unknown and changes nothing; a failing child is an error observation and the parent goes on; after a
restart on the same conversations directory the tool is still registered. The gateway is a stand-in
that answers the implementer and the child each from its own script.
"""
import http.server
import json
import os
import shutil
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

USAGE = {"prompt_tokens": 100, "completion_tokens": 5, "total_tokens": 105}
QUESTION = "Where is the function target defined in this repository?"


def tool_call(name, arguments):
    return {"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE, "choices": [
        {"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None, "tool_calls": [
            {"id": "c%d" % time.monotonic_ns(), "type": "function", "function": {"name": name, "arguments": json.dumps(arguments)}}]}}]}


class Gateway(http.server.BaseHTTPRequestHandler):
    """The child's calls carry the search brief in their system message; every other call is the
    implementer's. Each gets the next reply of its script (a dict, or an HTTP status)."""
    parent, child, bodies = [], [], []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        is_child = "read-only search helper" in json.dumps(body["messages"][0])
        Gateway.bodies.append(("child" if is_child else "parent", self.path, body))
        script = Gateway.child if is_child else Gateway.parent
        nxt = script.pop(0) if len(script) > 1 else script[0]
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


class SearchThroughAgentServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gateway = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=cls.gateway.serve_forever, daemon=True).start()
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        cls.port = probe.getsockname()[1]
        probe.close()
        cls.repo, cls.cwd = tempfile.mkdtemp(), tempfile.mkdtemp()
        env = {**os.environ, "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t",
               "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}
        with open(os.path.join(cls.repo, "a.py"), "w") as f:
            f.write("def target():\n    return 1\n")
        for args in (["init", "-q", "-b", "agent/x"], ["add", "a.py"], ["commit", "-q", "-m", "a"]):
            subprocess.run(["git", "-C", cls.repo, *args], check=True, env=env, capture_output=True)
        cls.patches = [mock.patch.object(agent_run, "AGENT_SERVER", "http://127.0.0.1:%d" % cls.port),
                       mock.patch.object(agent_run, "REPO_DIR", cls.repo)]
        for p in cls.patches:
            p.start()
        cls.start_server()

    @classmethod
    def start_server(cls):
        cmd = [sys.executable] + agent_run.SERVER_CMD[1:]
        cmd[cmd.index("--port") + 1] = str(cls.port)
        # One key across the restart, as a suspendable run keeps it (run_key); the ledger in the test's tree.
        env = agent_run.server_env({**{k: v for k, v in os.environ.items() if k != "PYTHONPATH"},
                                    "OH_SECRET_KEY": "k" * 43, "AGENT_SEARCH_LEDGER": os.path.join(cls.cwd, "search.jsonl")})
        cls.server = subprocess.Popen(cmd, cwd=cls.cwd, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        agent_run.wait_ready()

    @classmethod
    def stop_server(cls):
        cls.server.terminate()
        try:
            cls.server.wait(10)
        except subprocess.TimeoutExpired:
            cls.server.kill()
            cls.server.wait(10)

    @classmethod
    def tearDownClass(cls):
        cls.stop_server()
        cls.gateway.shutdown()
        cls.gateway.server_close()
        for p in cls.patches:
            p.stop()
        shutil.rmtree(cls.repo, ignore_errors=True)
        shutil.rmtree(cls.cwd, ignore_errors=True)

    def run_search(self, child, cid=None):
        Gateway.parent = [tool_call("search", {"question": QUESTION}), tool_call("finish", {"message": "done"})]
        Gateway.child, Gateway.bodies = child, []
        if cid is None:
            env = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:%d/v1" % self.gateway.server_port}
            request = agent_run.build_request(env, "fix the link", "rules", None, True)
            del request["conversation_id"]
            cid = agent_run.http("POST", "/api/conversations", request)["id"]
        else:
            agent_run.continue_conversation(cid, "again")
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            status = agent_run.http("GET", "/api/conversations/" + cid)["execution_status"]
            if status in ("finished", "error", "stuck"):
                break
            time.sleep(0.3)
        self.assertEqual(status, "finished")
        events = agent_run.http("GET", "/api/conversations/%s/events/search?limit=100" % cid)["items"]
        obs = [e["observation"] for e in events if e.get("kind") == "ObservationEvent" and e.get("tool_name") == "search"]
        return cid, obs[-1]

    def text(self, observation):
        return " ".join(c.get("text", "") for c in observation.get("content", []))

    def test_a_search_answers_on_the_same_v1_with_the_runs_model(self):
        _, obs = self.run_search([tool_call("grep", {"pattern": "def target"}),
                                  tool_call("finish", {"message": "Answer: a.py.\nReferences: a.py:1\nNot found: nothing"})])
        self.assertFalse(obs.get("is_error"), obs)
        self.assertIn("References: a.py:1", self.text(obs))
        child = [(p, b) for who, p, b in Gateway.bodies if who == "child"]
        self.assertEqual({p for p, _ in child}, {"/v1/chat/completions"})
        self.assertEqual({b["model"] for _, b in child}, {"agent-default"})
        self.assertEqual(sorted(t["function"]["name"] for t in child[0][1]["tools"]),
                         ["finish", "glob", "grep", "list_directory", "read_file", "think"])
        # The child's grep ran for real (ripgrep, the guard's absolute path) and found a.py. The SDK's grep
        # lists files, not lines, so a.py:1 is not in it.
        cls_repo = os.path.realpath(self.repo)
        grep_result = json.dumps([m for m in child[1][1]["messages"] if m.get("role") == "tool"])
        self.assertIn("Found 1 file(s) containing pattern", grep_result)
        self.assertIn(cls_repo + "/a.py", grep_result)
        last = [b for who, _, b in Gateway.bodies if who == "parent"][-1]
        self.assertRegex(json.dumps(last["messages"]), r"search [1-6]/6 \\u00b7 complete", "the parent received the answer and went on")

    def test_write_tools_are_unknown_to_the_child_and_change_nothing(self):
        _, obs = self.run_search([tool_call("terminal", {"command": "rm a.py"}),
                                  tool_call("file_editor", {"command": "create", "path": "b.py", "file_text": "x"}),
                                  tool_call("room_handoff", {"summary": "x"}),
                                  tool_call("finish", {"message": "Answer: none.\nReferences:\nNot found: target"})])
        self.assertFalse(obs.get("is_error"), obs)
        self.assertEqual(sorted(os.listdir(self.repo)), [".git", "a.py"])

    def test_a_child_answered_with_500s_is_an_error_and_the_parent_goes_on(self):
        _, obs = self.run_search([500])
        self.assertTrue(obs.get("is_error"), obs)
        self.assertIn("· error ·", self.text(obs))

    def test_search_survives_a_restart(self):
        cid, _ = self.run_search([tool_call("finish", {"message": "Answer: a.py:1"})])
        type(self).stop_server()
        type(self).start_server()
        _, obs = self.run_search([tool_call("finish", {"message": "Answer: still a.py:1"})], cid)
        self.assertFalse(obs.get("is_error"), obs)
        self.assertIn("still a.py:1", self.text(obs))


if __name__ == "__main__":
    unittest.main()
