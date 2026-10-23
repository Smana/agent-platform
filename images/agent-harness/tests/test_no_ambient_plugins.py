"""#187: a run conversation loads no repository or user plugin.

SDK 1.56's LocalConversation._ensure_plugins_loaded calls load_available_plugins(include_user=True,
include_project=True) with no switch, so a plugin under the clone's .agents/plugins (a hooks.json
shell command, an .mcp.json server) ran in the run, outside the bridge. site/sitecustomize.py
patch_ambient_plugins makes that call find nothing. Drives the SDK's real LocalConversation.
"""
import ast
import importlib.util
import inspect
import json
import os
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402
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

        from openhands.sdk.plugin import discovery

        self.mod = local_conversation
        # The SDK's own function: another test in the process may have run _install already.
        local_conversation.load_available_plugins = discovery.load_available_plugins
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
        # The patch rebinds a module global. If the SDK stops calling the global from
        # _ensure_plugins_loaded, or imports it locally there (which would bypass the rebinding),
        # the patch silently does nothing: fail here instead.
        src = textwrap.dedent(inspect.getsource(self.mod.LocalConversation._ensure_plugins_loaded))
        tree = ast.parse(src)
        calls = [n for n in ast.walk(tree) if isinstance(n, ast.Call)
                 and isinstance(n.func, ast.Name) and n.func.id == "load_available_plugins"]
        self.assertTrue(calls, "_ensure_plugins_loaded no longer calls load_available_plugins")
        local_imports = [n for n in ast.walk(tree) if isinstance(n, (ast.Import, ast.ImportFrom))
                         and any(a.name == "load_available_plugins" or a.asname == "load_available_plugins" for a in n.names)]
        self.assertEqual(local_imports, [], "the SDK imports load_available_plugins locally: the patch is bypassed")
        self.assertTrue(callable(getattr(self.mod, "load_available_plugins", None)))


class FailClosedTest(unittest.TestCase):
    def test_install_exits_when_the_patch_cannot_apply(self):
        from openhands.sdk.conversation.impl import local_conversation

        site = load_site()
        with mock.patch.dict(os.environ, {"PYTHONPATH": ""}), \
                mock.patch.object(local_conversation, "load_available_plugins", None), \
                mock.patch("sys.stderr", new=__import__("io").StringIO()):
            with self.assertRaises(SystemExit) as cm:
                site._install()
        self.assertIn("#187 plugin patch NOT applied", str(cm.exception))

    def test_install_exits_when_the_module_cannot_be_imported(self):
        # Another test in the process may have run _install already, so fail the import itself.
        import builtins

        real = builtins.__import__

        def refuse(name, globals=None, locals=None, fromlist=(), level=0):
            if "local_conversation" in (fromlist or ()) or name.endswith("local_conversation"):
                raise ImportError("no local_conversation")
            return real(name, globals, locals, fromlist, level)

        site = load_site()
        with mock.patch.dict(os.environ, {"PYTHONPATH": ""}), mock.patch("builtins.__import__", refuse), \
                mock.patch("sys.stderr", new=__import__("io").StringIO()):
            with self.assertRaises(SystemExit):
                site._install()

    def test_wait_ready_fails_at_once_when_agent_server_has_exited(self):
        proc = subprocess.Popen([sys.executable, "-c", "raise SystemExit(3)"])
        proc.wait()
        with mock.patch("agent_run.urllib.request.urlopen", side_effect=OSError), \
                mock.patch("agent_run.time.sleep") as sleep:
            with self.assertRaises(RuntimeError) as cm:
                agent_run.wait_ready(deadline_s=120, server=proc)
        self.assertIn("exited at start-up with code 3", str(cm.exception))
        sleep.assert_not_called()


class ServerEnvTest(unittest.TestCase):
    """A process started as agent-run starts agent-server (server_env: PYTHONPATH to site/)."""

    def test_repository_plugin_does_not_run_in_a_server_env_process(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        repo = os.path.join(d, "repo")
        os.makedirs(os.path.join(repo, ".git"))
        hook, mcp = os.path.join(d, "HOOK_RAN"), os.path.join(d, "MCP_RAN")
        evil_plugin(os.path.join(repo, ".agents", "plugins"), hook, mcp)
        probe = textwrap.dedent("""
            import sys
            from pydantic import SecretStr
            from openhands.sdk import LLM, Agent, Conversation
            conv = Conversation(agent=Agent(llm=LLM(model="openai/x", api_key=SecretStr("k")), tools=[]),
                                workspace=sys.argv[1], persistence_dir=sys.argv[2], visualizer=None)
            conv._ensure_plugins_loaded()
            try:
                conv._ensure_agent_ready()
            except Exception:
                pass
            conv.close()
        """)
        env = agent_run.server_env({k: v for k, v in os.environ.items() if k != "PYTHONPATH"})
        done = subprocess.run([sys.executable, "-c", probe, repo, os.path.join(d, "p")], env=env, cwd="/",
                              capture_output=True, text=True, timeout=180)
        self.assertEqual(done.returncode, 0, done.stderr)
        self.assertFalse(os.path.exists(hook))
        self.assertFalse(os.path.exists(mcp))


if __name__ == "__main__":
    unittest.main()
