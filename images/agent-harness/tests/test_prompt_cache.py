"""Anthropic prompt caching on internal runs.

The SDK marks its breakpoints with Anthropic's `cache_control`, but agentgateway 1.6 converts a Chat
Completions request to Messages from the OpenAI field only: a text part's
`prompt_cache_breakpoint: {"mode": "explicit"}` becomes `cache_control: {"type": "ephemeral"}`
(golden test prompt-cache-breakpoint.anthropic.snap), and `cache_control` is dropped. Drives the
SDK's real LLM, as agent-run configures it, against a stand-in gateway that records each body.
"""
import asyncio
import http.server
import importlib.util
import io
import json
import os
import sys
import threading
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)

import agent_run  # noqa: E402

SITE = os.path.join(HERE, "site", "sitecustomize.py")
BREAKPOINT = {"mode": "explicit"}
USAGE = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
OK = json.dumps({"id": "x", "object": "chat.completion", "created": 0, "model": "m", "usage": USAGE,
                 "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "ok"}}]}).encode()


class Gateway(http.server.BaseHTTPRequestHandler):
    bodies = []

    def do_POST(self):
        Gateway.bodies.append(json.loads(self.rfile.read(int(self.headers["content-length"]))))
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(OK)))
        self.end_headers()
        self.wfile.write(OK)

    def log_message(self, *args):
        pass


def load_site():
    spec = importlib.util.spec_from_file_location("harness_sitecustomize", SITE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def env(**extra):
    return {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:4000/v1",
            "MCP_URL": "http://127.0.0.1:1/mcp", **extra}


class RequestTest(unittest.TestCase):
    def overrides(self, **extra):
        return agent_run.build_request(env(**extra), "t", "r")["agent"]["llm"].get("capability_overrides", {})

    def test_an_internal_run_turns_the_sdk_cache_on(self):
        self.assertIs(self.overrides(DATA_CLASS="internal")["supports_prompt_cache"], True)

    def test_a_public_run_does_not(self):
        # Z.ai is a passthrough: an unknown part field would reach its API.
        self.assertNotIn("supports_prompt_cache", self.overrides(DATA_CLASS="public"))
        self.assertNotIn("supports_prompt_cache", self.overrides())


class BreakpointsTest(unittest.TestCase):
    def setUp(self):
        self.site = load_site()

    def test_a_part_marker_becomes_the_openai_breakpoint(self):
        messages = [{"role": "system", "content": [{"type": "text", "text": "s", "cache_control": {"type": "ephemeral"}}]}]
        self.site.cache_breakpoints(messages)
        self.assertEqual(messages, [{"role": "system", "content": [
            {"type": "text", "text": "s", "prompt_cache_breakpoint": BREAKPOINT}]}])

    def test_a_tool_message_marker_moves_onto_its_last_part(self):
        messages = [{"role": "tool", "tool_call_id": "c", "cache_control": {"type": "ephemeral"},
                     "content": [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]}]
        self.site.cache_breakpoints(messages)
        self.assertEqual(messages[0], {"role": "tool", "tool_call_id": "c", "content": [
            {"type": "text", "text": "a"}, {"type": "text", "text": "b", "prompt_cache_breakpoint": BREAKPOINT}]})

    def test_a_string_content_marker_becomes_a_text_part(self):
        messages = [{"role": "tool", "tool_call_id": "c", "cache_control": {"type": "ephemeral"}, "content": "out"}]
        self.site.cache_breakpoints(messages)
        self.assertEqual(messages[0]["content"], [{"type": "text", "text": "out", "prompt_cache_breakpoint": BREAKPOINT}])

    def test_unmarked_messages_are_untouched(self):
        messages = [{"role": "user", "content": [{"type": "text", "text": "hi"}]}, {"role": "user", "content": "x"}]
        before = json.loads(json.dumps(messages))
        self.site.cache_breakpoints(messages)
        self.assertEqual(messages, before)


class WireTest(unittest.TestCase):
    """What leaves the harness, through litellm, for the gateway to convert."""

    def setUp(self):
        from openhands.sdk import LLM

        site = load_site()
        for name in ("_transport_call", "_atransport_call"):
            self.addCleanup(setattr, LLM, name, getattr(LLM, name))
        self.assertTrue(site.patch_cache_breakpoints(LLM))
        server = http.server.HTTPServer(("127.0.0.1", 0), Gateway)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.base_url = "http://127.0.0.1:%d/v1" % server.server_port
        Gateway.bodies = []

    def send(self, data_class, run=lambda llm, m: llm.completion(messages=m)):
        from openhands.sdk import LLM, Message, TextContent
        from openhands.sdk.llm import MessageToolCall

        llm = LLM.model_validate(agent_run.build_request(
            env(LLM_BASE_URL=self.base_url, DATA_CLASS=data_class), "t", "r")["agent"]["llm"])
        call = MessageToolCall(id="c1", name="terminal", arguments="{}", origin="completion")
        run(llm, [
            Message(role="system", content=[TextContent(text="system prompt")]),
            Message(role="user", content=[TextContent(text="task")]),
            Message(role="assistant", content=[], tool_calls=[call]),
            Message(role="tool", tool_call_id="c1", name="terminal", content=[TextContent(text="output")]),
        ])
        self.assertEqual(len(Gateway.bodies), 1)
        return Gateway.bodies[0]["messages"]

    def assert_breakpoints(self, messages):
        self.assertNotIn("cache_control", json.dumps(messages), "1.6 drops it: no cache would be written")
        marked = [(m["role"], p["text"]) for m in messages if isinstance(m.get("content"), list)
                  for p in m["content"] if p.get("prompt_cache_breakpoint") == BREAKPOINT]
        # The SDK's two: the system prompt, and the conversation's newest turn.
        self.assertEqual(marked, [("system", "system prompt"), ("tool", "output")])

    def test_an_internal_call_carries_the_breakpoints(self):
        self.assert_breakpoints(self.send("internal"))

    def test_agent_servers_async_path_carries_them(self):
        self.assert_breakpoints(self.send("internal", run=lambda llm, m: asyncio.run(llm.agenerate(messages=m))))

    def test_a_public_call_carries_none(self):
        sent = json.dumps(self.send("public"))
        self.assertNotIn("prompt_cache_breakpoint", sent)
        self.assertNotIn("cache_control", sent)


class NotAppliedTest(unittest.TestCase):
    """The line AgentCacheMarkerMissing greps (observability/base/agent-platform/vmrule-logs.yaml in
    ai-native-ref): `prompt cache NOT applied`. A reworded line would silence the alert."""

    ALERT_GREP = "prompt cache NOT applied"

    def test_an_sdk_without_the_method_says_so_and_stays_unpatched(self):
        for missing in ("_transport_call", "_atransport_call"):
            with self.subTest(missing=missing):
                present = [n for n in ("_transport_call", "_atransport_call") if n != missing]
                llm = type("LLM", (), {n: (lambda self, **kw: None) for n in present})
                before = {n: getattr(llm, n) for n in present}
                with mock.patch("sys.stderr", new=io.StringIO()) as err:
                    self.assertFalse(load_site().patch_cache_breakpoints(llm))
                self.assertIn(self.ALERT_GREP, err.getvalue())
                self.assertEqual({n: getattr(llm, n) for n in present}, before)

    def test_an_sdk_that_cannot_be_imported_says_so_too(self):
        site = load_site()
        with mock.patch.dict(os.environ, {"PYTHONPATH": ""}), \
                mock.patch.dict(sys.modules, {"openhands.sdk.llm.llm": None, "mcp.client.streamable_http": None,
                                          "mcp.shared.session": None}), \
                mock.patch("sys.stderr", new=io.StringIO()) as err:
            site._install()
        self.assertIn(self.ALERT_GREP, err.getvalue())


if __name__ == "__main__":
    unittest.main()
