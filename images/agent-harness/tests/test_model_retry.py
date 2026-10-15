"""#99: a transient model error is retried, a 429 never is (OD-7).

Drives the SDK's real LLM, configured as agent-run configures it, against a stand-in for the gateway
that answers from a script: in process, sync and async, with the retry's sleep recorded instead of
slept; then through a real agent-server, as agent-run starts it.
"""
import asyncio
import http.server
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)

import agent_run  # noqa: E402

SITE = os.path.join(HERE, "site", "sitecustomize.py")
ENVOY = b"upstream connect error or disconnect/reset before headers. reset reason: remote connection failure"
USAGE = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
OK = json.dumps({"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE,
                 "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "ok"}}]}).encode()
FINISH = json.dumps({"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE, "choices": [
    {"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": None, "tool_calls": [
        {"id": "c1", "type": "function", "function": {"name": "finish", "arguments": json.dumps({"message": "done"})}}]}}]}).encode()


class Gateway(http.server.BaseHTTPRequestHandler):
    """Answers each request with the next entry of `script`: a status, or "reset" to drop the connection."""
    script, hits, ok = [], 0, OK

    def do_POST(self):
        self.rfile.read(int(self.headers["content-length"]))
        Gateway.hits += 1
        step = Gateway.script.pop(0) if Gateway.script else 200
        if step == "reset":
            self.close_connection = True
            return
        body = Gateway.ok if step == 200 else ENVOY
        self.send_response(step)
        self.send_header("content-type", "application/json" if step == 200 else "text/plain")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def load_site():
    spec = importlib.util.spec_from_file_location("harness_sitecustomize", SITE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ModelRetryTest(unittest.TestCase):
    def setUp(self):
        import openai
        from openhands.sdk import LLM

        self.site = load_site()
        for name in ("_transport_call", "_atransport_call"):
            self.addCleanup(setattr, LLM, name, getattr(LLM, name))
        self.sleeps = []

        async def asleep(seconds):
            self.sleeps.append(seconds)

        self.assertTrue(self.site.patch_model_retry(LLM, openai, sleep=self.sleeps.append, asleep=asleep))

        server = http.server.HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        env = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:%d/v1" % server.server_port,
               "MCP_URL": "http://127.0.0.1:1/mcp"}
        self.llm = LLM.model_validate(agent_run.build_request(env, "t", "r")["agent"]["llm"])
        self.assertFalse(self.llm.uses_responses_api(), "the retry wraps the chat-completions path only")
        self.retry_log = os.path.join(tempfile.mkdtemp(), "retries")
        patcher = mock.patch.dict(os.environ, {"AGENT_RUN_RETRY_LOG": self.retry_log})
        patcher.start()
        self.addCleanup(patcher.stop)

    def complete(self, messages):
        return self.llm.completion(messages=messages)

    def call(self, *script):
        from openhands.sdk import Message, TextContent

        Gateway.script, Gateway.hits, Gateway.ok = list(script), 0, OK
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            try:
                return self.complete([Message(role="user", content=[TextContent(text="hi")])]), err.getvalue()
            except Exception as exc:  # noqa: BLE001 -- the test asserts on it
                return exc, err.getvalue()

    def retries_logged(self):
        with mock.patch.object(agent_run, "RETRY_LOG", self.retry_log):
            return agent_run.model_retries()

    def test_a_503_then_success_continues(self):
        out, err = self.call(503)
        self.assertEqual(out.message.content[0].text, "ok")
        self.assertEqual((Gateway.hits, self.sleeps), (2, [10]))
        self.assertIn("agent-run model retry 1/2 in 10s: ServiceUnavailableError", err)
        self.assertEqual(self.retries_logged(), 1)

    def test_a_connection_reset_then_success_continues(self):
        out, err = self.call("reset")
        self.assertEqual(out.message.content[0].text, "ok")
        self.assertEqual((Gateway.hits, self.sleeps), (2, [10]))
        # litellm calls it a 500; the cause chain says connection.
        self.assertIn("InternalServerError", err)

    def test_three_503s_escalate_as_before(self):
        from openhands.sdk.llm.exceptions import LLMServiceUnavailableError

        out, _ = self.call(503, 503, 503)
        self.assertIsInstance(out, LLMServiceUnavailableError, "the error agent-run reported on 2026-10-15")
        self.assertIn("upstream connect error", str(out))
        self.assertEqual((Gateway.hits, self.sleeps), (3, [10, 20]))
        self.assertLessEqual(sum(self.sleeps), 60, "OD-7's bound")
        self.assertEqual(self.retries_logged(), 2)

    def test_a_429_is_never_retried(self):
        from openhands.sdk.llm.exceptions import LLMRateLimitError

        out, err = self.call(429, 200)
        self.assertIsInstance(out, LLMRateLimitError)
        self.assertEqual((Gateway.hits, self.sleeps), (1, []), "neither we, the SDK nor litellm's client retry it")
        self.assertNotIn("model retry", err)
        self.assertEqual(self.retries_logged(), 0)


class AsyncModelRetryTest(ModelRetryTest):
    """agent-server's path: Agent.astep calls LLM.agenerate, which ends in _atransport_call."""

    def complete(self, messages):
        return asyncio.run(self.llm.agenerate(messages=messages))


class AgentServerTest(unittest.TestCase):
    """The patch as agent-server loads it, end to end: agent-run's own request, poll and summary."""

    @classmethod
    def setUpClass(cls):
        cls.gateway = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=cls.gateway.serve_forever, daemon=True).start()
        cls.retry_log = os.path.join(tempfile.mkdtemp(), "retries")
        cls.patcher = mock.patch.object(agent_run, "RETRY_LOG", cls.retry_log)
        cls.patcher.start()
        env = agent_run.server_env({k: v for k, v in os.environ.items() if k != "PYTHONPATH"})
        cls.stderr = tempfile.TemporaryFile("w+")
        cls.server = subprocess.Popen(agent_run.SERVER_CMD, cwd=tempfile.mkdtemp(), env=env,
                                      stdout=subprocess.DEVNULL, stderr=cls.stderr)
        agent_run.wait_ready()
        os.makedirs(agent_run.REPO_DIR, exist_ok=True)

    @classmethod
    def tearDownClass(cls):
        cls.server.terminate()
        cls.server.wait(10)
        cls.gateway.shutdown()
        cls.gateway.server_close()
        cls.patcher.stop()

    def run_with(self, *script):
        open(self.retry_log, "w").close()
        Gateway.script, Gateway.hits, Gateway.ok = list(script), 0, FINISH
        env = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:%d/v1" % self.gateway.server_port,
               "MCP_URL": "http://127.0.0.1:1/mcp"}
        request = agent_run.build_request(env, "say done", "rules")
        del request["agent"]["mcp_config"], request["conversation_id"]
        cid = agent_run.http("POST", "/api/conversations", request)["id"]
        steps = agent_run.StepLog(cid)
        with mock.patch.object(agent_run, "POLL_INTERVAL_S", 0.5), \
                mock.patch("sys.stdout", new=io.StringIO()) as out, mock.patch("sys.stderr", new=io.StringIO()):
            code = agent_run.poll(cid, on_tick=steps)
            steps()
            steps.summary()
        return code, out.getvalue()

    def test_a_503_mid_run_is_retried_and_the_run_finishes(self):
        code, out = self.run_with(503)
        self.assertEqual((code, Gateway.hits), (0, 2), out)
        self.assertIn("agent-run summary: 1 steps, 1 model retries", out)

    def test_three_503s_end_the_run_with_the_same_error(self):
        code, out = self.run_with(503, 503, 503)
        self.assertEqual((code, Gateway.hits), (1, 3), out)
        self.assertRegex(out, r"agent-run error: LLMServiceUnavailableError .*upstream connect error")
        self.assertIn("agent-run summary: 0 steps, 2 model retries", out)


class TransientTest(unittest.TestCase):
    def test_what_is_retried(self):
        import httpx
        import litellm
        import openai

        site = load_site()
        req = httpx.Request("POST", "http://gateway/v1/chat/completions")

        def status(code, message="x"):
            return openai.APIStatusError(message, response=httpx.Response(code, request=req), body=None)

        cases = {
            "a 503": (status(503), True),
            "the Envoy reset under another status": (litellm.InternalServerError(ENVOY.decode(), "openai", "m"), True),
            "a refused connection": (openai.APIConnectionError(request=req), True),
            "a 429": (litellm.RateLimitError("budget", "openai", "m"), False),
            "a 429 that says upstream reset": (status(429, ENVOY.decode()), False),
            "a timeout": (litellm.Timeout("slow", "m", "openai"), False),
            "a 500": (status(500), False),
            "a 400": (status(400), False),
        }
        for name, (exc, want) in cases.items():
            with self.subTest(name):
                self.assertEqual(site.transient(exc, openai), want)


class InstallTest(unittest.TestCase):
    def test_agent_server_loads_the_retry(self):
        env = agent_run.server_env({k: v for k, v in os.environ.items() if k != "PYTHONPATH"})
        probe = "from openhands.sdk.llm.llm import LLM; print(LLM._transport_call.__module__, LLM._atransport_call.__module__)"
        done = subprocess.run([sys.executable, "-c", probe], env=env, cwd="/", capture_output=True, text=True, timeout=120)
        self.assertEqual(done.stdout.split(), ["sitecustomize"] * 2, done.stderr)
        self.assertEqual(env["AGENT_RUN_RETRY_LOG"], agent_run.RETRY_LOG)

    def test_a_missing_method_is_said_loudly(self):
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            self.assertFalse(load_site().patch_model_retry(type("LLM", (), {}), None))
        self.assertIn("#99 retry NOT applied", err.getvalue())

    def test_the_summary_counts_retries(self):
        path = os.path.join(tempfile.mkdtemp(), "retries")
        with open(path, "w") as f:
            f.write("agent-run model retry 1/2 in 10s: a\nagent-run model retry 2/2 in 20s: b\n")
        with mock.patch.object(agent_run, "RETRY_LOG", path), mock.patch("sys.stdout", new=io.StringIO()) as out:
            agent_run.StepLog("cid").summary()
        self.assertIn("agent-run summary: 0 steps, 2 model retries", out.getvalue())


if __name__ == "__main__":
    unittest.main()
