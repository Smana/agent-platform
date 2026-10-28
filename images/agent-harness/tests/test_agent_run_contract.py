"""The test-first tester in agent-run (v3 feature 4, F1): the tester's red check, signed push and ends,
the implementer's contract, hook and hold. Offline proofs 2 and 3 on real repositories and a local bare
remote; the conversation is scripted as in test_loop_run.DriveTest."""
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_run  # noqa: E402
import loop_policy as lp  # noqa: E402
import test_contract as tc  # noqa: E402
from test_test_contract import RUN_SH, SHELL_DECL  # noqa: E402

GIT_ENV = {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x",
           "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"}


def git(cwd, *args):
    return subprocess.run(["git", "-C", cwd, *args], check=True, capture_output=True, text=True).stdout.strip()


class Origin:
    """A bare origin whose main declares shell-suite tests, a clone on agent/x as clone() leaves it."""
    files = {".github/agent-checks.yaml": SHELL_DECL, "t/run.sh": RUN_SH,
             "bin/greet": "#!/usr/bin/env bash\necho hello\n",
             "t/test-greet.sh": 'out="$(bash bin/greet)"; [ "$out" = hello ]\n'}

    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d)
        mock.patch.dict(os.environ, GIT_ENV).start()
        self.addCleanup(mock.patch.stopall)
        self.origin, seed = os.path.join(self.d, "origin.git"), os.path.join(self.d, "seed")
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", self.origin], check=True)
        subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True)
        self.write(seed, self.files)
        git(seed, "add", "-A")
        git(seed, "commit", "-qm", "base")
        git(seed, "remote", "add", "origin", self.origin)
        git(seed, "push", "-q", "origin", "main")
        self.repo = os.path.join(self.d, "repo")
        subprocess.run(["git", "clone", "-q", self.origin, self.repo], check=True)
        git(self.repo, "checkout", "-q", "-B", "agent/x", "origin/main")
        mock.patch.object(agent_run, "REPO_DIR", self.repo).start()
        mock.patch.object(agent_run, "CHECKS_DIR", os.path.join(self.d, "checks")).start()
        mock.patch.object(agent_run, "CONTRACT_FILE", os.path.join(self.d, "contract.json")).start()
        self.env = {"ROLE": "tester", "BRANCH": "agent/x", "BASE_REF": "main", "REPOSITORY": "o/r",
                    "RUN_ID": "run-t", "TASK_ID": "task-1"}
        self.base = git(self.repo, "rev-parse", "origin/main")

    def write(self, root, files):
        for path, body in files.items():
            full = os.path.join(root, path)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "w") as f:
                f.write(body)
            if path.endswith(".sh") or path.startswith("bin/"):
                os.chmod(full, 0o755)

    def commit(self, files, msg="tests"):
        self.write(self.repo, files)
        git(self.repo, "add", "-A")
        git(self.repo, "-c", "core.hooksPath=/dev/null", "commit", "-qm", msg)


class ModeTest(unittest.TestCase):
    def test_a_tester_without_a_task_url_writes_tests(self):
        self.assertTrue(agent_run.test_first({"ROLE": "tester"}))

    def test_the_verifier_tester_and_every_other_role_are_todays(self):
        self.assertFalse(agent_run.test_first({"ROLE": "tester", "TASK_URL": "https://github.com/o/r/pull/1"}))
        for role in ("implementer", "reviewer", "triager", ""):
            self.assertFalse(agent_run.test_first({"ROLE": role}))


class TesterCloneTest(Origin, unittest.TestCase):
    def test_the_testers_clone_cannot_push(self):
        agent_run.disable_push(self.env)
        self.commit({"t/test-name.sh": "exit 1\n"})
        p = subprocess.run(["git", "-C", self.repo, "push", "origin", "HEAD:refs/heads/agent/x"], capture_output=True, text=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertEqual(git(self.repo, "remote", "get-url", "origin"), self.origin, "the fetch URL stays the harness's")


class ToolchainTest(Origin, unittest.TestCase):
    def test_contract_env_waits_for_the_prefetch_then_installs_once(self):
        ac = agent_run.checks_module()
        proc = mock.Mock()
        order = []
        proc.wait.side_effect = lambda timeout=None: order.append(("wait", timeout))
        with mock.patch.object(ac, "pins", return_value={"go": "1.2"}) as pins, \
                mock.patch.object(ac, "install", side_effect=lambda t, say: order.append("install") or []) as install, \
                mock.patch.object(ac, "step_env", return_value={"PATH": "/x"}) as step_env:
            env = agent_run.contract_env({"tools": ["go"]}, proc)
        self.assertEqual(order, [("wait", ac.INSTALL_S), "install"])
        pins.assert_called_once_with(["go"])
        self.assertEqual(install.call_count, 1)
        step_env.assert_called_once_with({"go": "1.2"}, [])
        trees = os.path.join(agent_run.CHECKS_DIR, "trees")
        self.assertEqual(env["TMPDIR"], trees)
        self.assertTrue(os.path.isdir(trees))
        self.assertEqual(env["PATH"], "/x")

    def test_a_prefetch_that_overruns_is_not_waited_for_twice(self):
        ac = agent_run.checks_module()
        proc = mock.Mock()
        proc.wait.side_effect = subprocess.TimeoutExpired("x", 1)
        with mock.patch.object(ac, "pins", return_value={}), mock.patch.object(ac, "install", return_value=[]), \
                mock.patch.object(ac, "step_env", return_value={}):
            self.assertIn("TMPDIR", agent_run.contract_env({}, proc))

    def test_no_prefetch_is_fine(self):
        ac = agent_run.checks_module()
        with mock.patch.object(ac, "pins", return_value={}), mock.patch.object(ac, "install", return_value=[]), \
                mock.patch.object(ac, "step_env", return_value={}):
            self.assertIn("TMPDIR", agent_run.contract_env({}, None))

    def test_the_start_base_is_the_clones_default_branch_and_does_not_move(self):
        self.assertEqual(agent_run.start_base(), self.base)
        self.commit({"t/other.sh": "exit 0\n"})
        other = git(self.repo, "rev-parse", "HEAD")
        start = agent_run.start_base()
        git(self.repo, "update-ref", "refs/remotes/origin/HEAD", other)
        self.assertEqual(start, self.base)
        self.assertEqual(agent_run.start_base(), other, "a later read would move: the harness reads once")


def say(m):
    pass


class RedCheckTest(Origin, unittest.TestCase):
    RED = {"t/test-name.sh": 'out="$(bash bin/greet ada)"; [ "$out" = "hello ada" ]\n'}

    def check(self):
        return agent_run.red_check(dict(os.environ), self.base, dict(os.environ))

    def origin_log(self, fmt, ref="agent/x"):
        return git(self.origin, "log", "-1", "--format=" + fmt, ref)

    def test_a_red_suite_is_signed_and_pushed(self):
        self.commit(self.RED)
        code, res = self.check()
        self.assertEqual((code, res["result"]), (tc.OK, "contract_ready"))
        self.assertIsNone(agent_run.push_contract(self.env, agent_run.sign(self.env, self.base, res)))
        self.assertEqual(self.origin_log("%(trailers:key=Agent-Contract,valueonly)"), "run-t")
        self.assertEqual(self.origin_log("%(trailers:key=Agent-Contract-Id,valueonly)"), "t/test-name.sh")
        self.assertEqual(self.origin_log("%(trailers:key=Agent-Run,valueonly)"), "run-t")
        self.assertEqual(self.origin_log("%(trailers:key=Agent-Task,valueonly)"), "task-1")
        self.assertEqual(self.origin_log("%s"), agent_run.CONTRACT_SUBJECT)
        self.assertEqual(git(self.origin, "rev-list", "--count", "main..agent/x"), "1")
        self.assertEqual(self.origin_log("%P"), self.base)

    def test_two_commits_are_squashed_onto_the_base(self):
        self.commit(self.RED, "one")
        self.commit({"t/test-other.sh": "exit 1\n"}, "two")
        code, res = self.check()
        self.assertEqual(res["result"], "contract_ready")
        self.assertIsNone(agent_run.push_contract(self.env, agent_run.sign(self.env, self.base, res)))
        self.assertEqual(git(self.origin, "rev-list", "--count", "main..agent/x"), "1")
        files = self.origin_log("%(trailers:key=Agent-Contract-File,valueonly)").split("\n")
        self.assertEqual(sorted(f for f in files if f), ["t/test-name.sh", "t/test-other.sh"])

    def test_nothing_committed_is_not_testable(self):
        self.assertIsNone(agent_run.squash(self.base, agent_run.PROVISIONAL))
        code, res = self.check()
        self.assertEqual(res["result"], "not_testable")
        self.assertNotEqual(git(self.origin, "branch", "--list", "agent/x"), "agent/x")

    def test_a_file_outside_the_test_paths_is_outside(self):
        self.commit({**self.RED, "bin/greet": "#!/usr/bin/env bash\necho changed\n"})
        code, res = self.check()
        self.assertEqual((code, res["result"]), (tc.OUTSIDE, "contract_outside_tests"))
        self.assertEqual(git(self.origin, "branch", "--list", "agent/x"), "")

    def test_a_passing_suite_is_not_red(self):
        self.commit({"t/test-vacuous.sh": "exit 0\n"})
        code, res = self.check()
        self.assertEqual(res["result"], "contract_not_red")
        self.assertIn("t/test-vacuous.sh", res["why"])

    def test_a_refused_push_is_reported_not_raised(self):
        self.commit(self.RED)
        _, res = self.check()
        sha = agent_run.sign(self.env, self.base, res)
        refs = os.path.join(self.origin, "refs")
        subprocess.run(["chmod", "-R", "a-w", refs], check=True)
        self.addCleanup(subprocess.run, ["chmod", "-R", "u+w", refs])
        if os.access(refs, os.W_OK):
            self.skipTest("running as a user the permissions do not bind")
        why = agent_run.push_contract(self.env, sha)
        self.assertIsInstance(why, str)
        self.assertTrue(why)

    def test_trailer_values_carry_no_newline(self):
        self.commit(self.RED)
        res = {"files": ["t/test-name.sh"], "ids": ["t/a\nb: x", "t/c"]}
        sha = agent_run.sign(self.env, self.base, res)
        msg = git(self.repo, "show", "-s", "--format=%B", sha)
        parsed = subprocess.run(["git", "interpret-trailers", "--parse"], input=msg, capture_output=True, text=True).stdout.splitlines()
        ids = [t.split(": ", 1)[1] for t in parsed if t.startswith("Agent-Contract-Id")]
        self.assertEqual(ids, ["t/a b: x", "t/c"])
