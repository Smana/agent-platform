"""cloud-native-ref#2283: each MCP call agent-server makes carries the traceparent of the span that
made it, so the gateway's `tools/call` span nests under that tool call instead of under the run.

Runs the real MCP client (fastmcp over mcp's streamable HTTP, as the SDK does) against a stand-in for
the gateway that records the traceparent of every POST.
"""
import http.server
import importlib.util
import io
import json
import os
import subprocess
import sys
import threading
import types
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)

import agent_run  # noqa: E402

# The run span's header, as build_request puts it on the platform MCP entry.
STATIC = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"


class Gateway(http.server.BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Gateway.seen.append((msg.get("method"), self.headers.get("traceparent")))
        if "id" not in msg:
            self.send_response(202)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        result = {"initialize": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                                 "serverInfo": {"name": "gateway", "version": "0"}},
                  "tools/list": {"tools": [{"name": "echo", "inputSchema": {"type": "object"}}]},
                  "tools/call": {"content": [{"type": "text", "text": "pong"}], "isError": False}}.get(msg["method"], {})
        body = json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}).encode()
        self.send_response(200)
        for key, value in (("Content-Type", "application/json"), ("mcp-session-id", "s1")):
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.send_response(405)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_DELETE(self):
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args):
        pass


# Two tool calls, each inside its own root span, as MCPToolExecutor.call_tool's @observe makes
# current. Each step is its own trace, so a header in a step's trace was made under that step.
# Prints each step span's traceparent.
CLIENT = r"""
import asyncio, json, os, sys
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from fastmcp import Client
from fastmcp.client.transports import StreamableHttpTransport

# Real spans, so fastmcp's own client span sits between the step and the header, as live.
trace.set_tracer_provider(TracerProvider())

async def main(url, static):
    steps = []
    async with Client(StreamableHttpTransport(url, headers={"traceparent": static})) as client:
        for _ in range(2):
            with trace.get_tracer("test").start_as_current_span("MCPToolExecutor.call_tool") as span:
                sc = span.get_span_context()
                steps.append("00-%032x-%016x-01" % (sc.trace_id, sc.span_id))
                await asyncio.wait_for(client.call_tool_mcp("echo", {}), 5)
    print(json.dumps({"steps": steps}), flush=True)
    os._exit(0)

asyncio.run(main(sys.argv[1], sys.argv[2]))
"""


class PerCallTraceparentTest(unittest.TestCase):
    def calls(self, env):
        Gateway.seen = []
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        server.daemon_threads = True
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        url = "http://127.0.0.1:%d/mcp" % server.server_port
        done = subprocess.run([sys.executable, "-c", CLIENT, url, STATIC], env=env, cwd="/",
                              capture_output=True, text=True, timeout=60)
        self.assertEqual(done.returncode, 0, done.stderr)
        out = json.loads(done.stdout.strip().splitlines()[-1])
        return out, [tp for method, tp in Gateway.seen if method == "tools/call"]

    def env(self):
        env = dict(os.environ)
        env.pop("PYTHONPATH", None)
        return env

    def test_each_call_carries_the_span_that_made_it(self):
        out, sent = self.calls(agent_run.server_env(self.env()))
        self.assertEqual(len(sent), 2, Gateway.seen)
        steps = [step.split("-")[1] for step in out["steps"]]
        self.assertNotEqual(steps[0], steps[1], "each step is its own trace")
        for header in sent:
            self.assertRegex(header, agent_run.TRACEPARENT)
        self.assertEqual([header.split("-")[1] for header in sent], steps, "each call is in its step's trace")

    def test_unpatched_every_call_carries_the_run_span(self):
        # The gap the patch closes: mcp sends the config's headers on every request.
        _, sent = self.calls(self.env())
        self.assertEqual(sent, [STATIC, STATIC])


class PatchTest(unittest.TestCase):
    def test_a_missing_hook_is_said_loudly(self):
        spec = importlib.util.spec_from_file_location("harness_sitecustomize", os.path.join(HERE, "site", "sitecustomize.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            self.assertFalse(module.patch_traceparent(types.SimpleNamespace(), type("Transport", (), {}), None))
        self.assertIn("#2283 traceparent NOT applied", err.getvalue())


if __name__ == "__main__":
    unittest.main()
