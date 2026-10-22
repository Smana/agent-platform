"""The SDK's own MCP client, as agent-server drives it, after a gateway refuses one tool call.

test_mcp_ping drives fastmcp directly; this drives openhands.sdk's MCPClient and MCPToolExecutor, the
code that decides whether a refused call costs the run its MCP tools. SDK 1.49.6 disconnected for the
rest of the run on an HTTP 403 to a tools/call (capability profiles spike S0); 1.55 reconnects
(software-agent-sdk#4838). The deny design may rely on either only while this test says so.
"""
import http.server
import json
import os
import subprocess
import sys
import threading
import unittest

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)

import agent_run  # noqa: E402


class Gateway(http.server.BaseHTTPRequestHandler):
    """An MCP server whose first tools/call is refused as Gateway.refusal says; later ones succeed."""
    refusal, calls = "http-403", 0

    def answer(self, code, body=b"", headers=()):
        self.send_response(code)
        for key, value in (("Content-Type", "application/json"), *headers):
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        msg = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        method = msg.get("method")
        session = [("mcp-session-id", "s1")]
        if method == "tools/call":
            Gateway.calls += 1
            if Gateway.calls == 1 and Gateway.refusal == "http-403":
                return self.answer(403, b'{"error": "denied by policy"}')
            if Gateway.calls == 1:
                body = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32000, "message": "denied"}}
                return self.answer(200, json.dumps(body).encode(), session)
        result = {"initialize": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}},
                                 "serverInfo": {"name": "gateway", "version": "0"}},
                  "tools/list": {"tools": [{"name": "echo", "description": "d",
                                            "inputSchema": {"type": "object", "properties": {}}}]},
                  "tools/call": {"content": [{"type": "text", "text": "pong"}], "isError": False}}.get(method, {})
        if "id" not in msg:
            return self.answer(202)
        self.answer(200, json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": result}).encode(), session)

    def do_GET(self):
        self.answer(405)

    def do_DELETE(self):
        self.answer(200)

    def log_message(self, *args):
        pass


# Three calls through the SDK's executor; prints one JSON line of each observation's outcome.
CLIENT = r"""
import json, os, sys
os.environ["OPENHANDS_SUPPRESS_BANNER"] = "1"
from openhands.sdk.mcp.config import MCPServer
from openhands.sdk.mcp.definition import MCPToolAction
from openhands.sdk.mcp.utils import create_mcp_tools

client = create_mcp_tools({"platform": MCPServer.model_validate({"url": sys.argv[1], "transport": "http"})}, timeout=15)
executor = client.tools[0].executor
outcomes = []
for _ in range(3):
    obs = executor(MCPToolAction(data={}))
    outcomes.append({"is_error": bool(obs.is_error), "text": obs.text})
print(json.dumps(outcomes), flush=True)
os._exit(0)
"""


class RefusedCallTest(unittest.TestCase):
    def calls_after(self, refusal):
        Gateway.refusal, Gateway.calls = refusal, 0
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Gateway)
        server.daemon_threads = True
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        env = {k: v for k, v in os.environ.items() if k != "PYTHONPATH"}
        done = subprocess.run([sys.executable, "-c", CLIENT, "http://127.0.0.1:%d/mcp" % server.server_port],
                              env=env, cwd="/", capture_output=True, text=True, timeout=90)
        self.assertEqual(done.returncode, 0, done.stderr[-2000:])
        return json.loads(done.stdout.strip().splitlines()[-1])

    def assert_only_the_first_is_refused(self, outcomes):
        self.assertEqual([o["is_error"] for o in outcomes], [True, False, False], outcomes)
        self.assertIn("pong", outcomes[2]["text"])
        self.assertEqual(Gateway.calls, 3, "every call reached the server")

    def test_a_jsonrpc_error_keeps_the_session(self):
        self.assert_only_the_first_is_refused(self.calls_after("jsonrpc"))

    def test_an_http_403_keeps_the_session_from_sdk_1_55(self):
        # 1.49.6: the later calls fail "MCP client not connected ... Reconnection attempt failed".
        self.assert_only_the_first_is_refused(self.calls_after("http-403"))


if __name__ == "__main__":
    unittest.main()
