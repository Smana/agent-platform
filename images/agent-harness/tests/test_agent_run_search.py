"""agent-run's half of search as a tool (v3 feature 5, offline proof 3)."""
import json
import os
import sys
import unittest

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402

GOLDEN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "golden", "start-request.json")
ENV = {"MODEL": "agent-default", "LLM_BASE_URL": "http://127.0.0.1:4000/v1", "MCP_URL": "http://127.0.0.1:4000/mcp",
       "CONVERSATION_ID": "0b3c6f0e-5d1a-4b8e-9f41-2a7c3e9d8b10", "ROLE": "implementer"}


class SwitchTest(unittest.TestCase):
    def test_todays_request_is_the_golden(self):
        with open(GOLDEN) as f:
            self.assertEqual(json.dumps(agent_run.build_request(ENV, "fix the link", "rules text"), indent=1, sort_keys=True) + "\n", f.read())


if __name__ == "__main__":
    unittest.main()
