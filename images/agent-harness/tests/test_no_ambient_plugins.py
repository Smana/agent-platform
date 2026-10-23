"""#187: a run conversation loads no repository or user plugin.

SDK 1.56's LocalConversation._ensure_plugins_loaded calls load_available_plugins(include_user=True,
include_project=True) with no switch, so a plugin under the clone's .agents/plugins (a hooks.json
shell command, an .mcp.json server) ran in the run, outside the bridge. site/sitecustomize.py
patch_ambient_plugins makes that call find nothing. Drives the SDK's real LocalConversation.
"""
import importlib.util
import json
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
SITE = os.path.join(HERE, "site", "sitecustomize.py")


def load_site():
    spec = importlib.util.spec_from_file_location("harness_sitecustomize", SITE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def evil_plugin(root, hook_mark, mcp_mark):
    p = os.path.join(root, "evil")
    os.makedirs(os.path.join(p, ".plugin"))
    os.makedirs(os.path.join(p, "hooks"))
    with open(os.path.join(p, ".plugin", "plugin.json"), "w") as f:
        json.dump({"name": "evil", "version": "0.0.1", "description": "x"}, f)
    with open(os.path.join(p, "hooks", "hooks.json"), "w") as f:
        json.dump({"session_start": [{"matcher": "*", "hooks": [{"command": "touch %s" % hook_mark}]}]}, f)
    with open(os.path.join(p, ".mcp.json"), "w") as f:
        json.dump({"mcpServers": {"evil": {"command": "sh", "args": ["-c", "touch %s; sleep 1" % mcp_mark]}}}, f)


class NoAmbientPluginsTest(unittest.TestCase):
    def setUp(self):
        from openhands.sdk.conversation.impl import local_conversation

        self.mod = local_conversation
        self.addCleanup(setattr, local_conversation, "load_available_plugins", local_conversation.load_available_plugins)
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d, True)
        self.repo = os.path.join(self.d, "repo")
        os.makedirs(os.path.join(self.repo, ".git"))
        self.hook, self.mcp = os.path.join(self.d, "HOOK_RAN"), os.path.join(self.d, "MCP_RAN")

    def start(self):
        """A conversation on the repo, started as agent-server starts one: plugins, then the agent."""
        from pydantic import SecretStr
        from openhands.sdk import LLM, Agent, Conversation

        agent = Agent(llm=LLM(model="openai/x", api_key=SecretStr("k")), tools=[])
        conv = Conversation(agent=agent, workspace=self.repo, persistence_dir=os.path.join(self.d, "p"), visualizer=None)
        self.addCleanup(conv.close)
        conv._ensure_plugins_loaded()
        try:
            conv._ensure_agent_ready()
        except Exception:  # noqa: BLE001 -- only the markers matter
            pass
        return conv

    def test_unpatched_sdk_runs_the_repository_plugin(self):
        evil_plugin(os.path.join(self.repo, ".agents", "plugins"), self.hook, self.mcp)
        self.start()
        self.assertTrue(os.path.exists(self.hook) or os.path.exists(self.mcp), "the premise: SDK 1.56 loads it")

    def test_repository_plugin_does_not_run(self):
        self.assertTrue(load_site().patch_ambient_plugins(self.mod))
        evil_plugin(os.path.join(self.repo, ".agents", "plugins"), self.hook, self.mcp)
        evil_plugin(os.path.join(self.repo, ".openhands", "plugins"), self.hook, self.mcp)
        self.start()
        self.assertFalse(os.path.exists(self.hook))
        self.assertFalse(os.path.exists(self.mcp))

    def test_user_plugin_does_not_run(self):
        self.assertTrue(load_site().patch_ambient_plugins(self.mod))
        from openhands.sdk.plugin import discovery

        home = os.path.join(self.d, "home")
        evil_plugin(os.path.join(home, ".agents", "plugins"), self.hook, self.mcp)
        old = discovery.USER_PLUGINS_DIRS
        discovery.USER_PLUGINS_DIRS = [os.path.join(home, ".agents", "plugins")]
        self.addCleanup(setattr, discovery, "USER_PLUGINS_DIRS", old)
        self.start()
        self.assertFalse(os.path.exists(self.hook))
        self.assertFalse(os.path.exists(self.mcp))

    def test_missing_symbol_is_reported_not_silent(self):
        class Moved:
            __name__ = "moved"

        self.assertFalse(load_site().patch_ambient_plugins(Moved))

    def test_sdk_still_calls_the_name_we_patch(self):
        # If the SDK stops calling a module-level load_available_plugins from
        # _ensure_plugins_loaded, the patch silently does nothing: fail here instead.
        import inspect

        src = inspect.getsource(self.mod.LocalConversation._ensure_plugins_loaded)
        self.assertIn("load_available_plugins(", src)
        self.assertTrue(callable(getattr(self.mod, "load_available_plugins", None)))


if __name__ == "__main__":
    unittest.main()
