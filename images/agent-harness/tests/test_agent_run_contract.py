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
