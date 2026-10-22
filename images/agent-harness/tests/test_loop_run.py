"""agent_run's half of the loop policy (loops design L2, L3): the commit count and the stall rule on
a real clone, the checks and the judge's call against stand-ins, the drive, and the end's report.
Needs the openhands SDK for the message shape, so it runs in the image: docker build --target test.
"""
import http.server
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402
import loop_policy as lp  # noqa: E402

POLICY = lp.Policy(stall_steps=80, continuations=2, judge_model="tier-light")
BRIEF = "TASK-DATA-n1\n## Acceptance check\n`task docs-links` passes.\nTASK-DATA-n1\n"
RUNS = [{"name": "check", "status": "completed", "conclusion": "success", "title": "All green"},
        {"name": "lint", "status": "in_progress", "conclusion": "", "title": ""}]


def git(cwd, *args):
    return subprocess.run(["git", "-C", cwd, *args], check=True, capture_output=True, text=True).stdout.strip()


class Clone:
    """origin with main, and the run's branch checked out from it as clone() does."""

    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.d)
        origin, seed = os.path.join(self.d, "origin.git"), os.path.join(self.d, "seed")
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", origin], check=True)
        subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True)
        mock.patch.dict(os.environ, {"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t",
                                     "GIT_COMMITTER_EMAIL": "t@x", "GIT_CONFIG_GLOBAL": os.devnull,
                                     "GIT_CONFIG_NOSYSTEM": "1"}).start()
        self.addCleanup(mock.patch.stopall)
        self.commit(seed, "base")
        git(seed, "remote", "add", "origin", origin)
        git(seed, "push", "-q", "origin", "main")
        self.seed, self.repo = seed, os.path.join(self.d, "repo")
        subprocess.run(["git", "clone", "-q", origin, self.repo], check=True)
        git(self.repo, "checkout", "-q", "-B", "agent/x", "origin/main")
        mock.patch.object(agent_run, "REPO_DIR", self.repo).start()
        self.env = {"BRANCH": "agent/x", "BASE_REF": "main", "REPOSITORY": "o/r"}
        self.start = agent_run.head()

    def commit(self, cwd, name):
        with open(os.path.join(cwd, name), "w") as f:
            f.write(name)
        git(cwd, "add", name)
        git(cwd, "commit", "-q", "-m", name)


class CommitsTest(Clone, unittest.TestCase):
    def test_the_agents_own_commits_count(self):
        self.assertEqual(agent_run.commits_since(self.env, self.start), 0)
        self.commit(self.repo, "a")
        self.assertEqual(agent_run.commits_since(self.env, self.start), 1)

    def test_a_rebase_onto_a_newer_base_counts_nothing(self):
        self.commit(self.seed, "upstream")
        git(self.seed, "push", "-q", "origin", "main")
        git(self.repo, "pull", "-q", "--rebase", "origin", "main")
        self.assertEqual(agent_run.commits_since(self.env, self.start), 0)

    def test_a_merge_of_the_base_counts_nothing_but_the_agents_commit_does(self):
        self.commit(self.repo, "a")
        self.commit(self.seed, "upstream")
        git(self.seed, "push", "-q", "origin", "main")
        git(self.repo, "fetch", "-q", "origin")
        git(self.repo, "merge", "-q", "--no-edit", "origin/main")
        self.assertEqual(agent_run.commits_since(self.env, self.start), 1)

    def test_a_commit_on_the_branch_counts_while_another_is_checked_out(self):
        self.commit(self.repo, "a")
        git(self.repo, "checkout", "-q", "origin/main")
        self.assertEqual(agent_run.commits_since(self.env, self.start), 1)

    def test_an_unknown_mark_or_a_hung_git_is_no_answer(self):
        self.assertIsNone(agent_run.commits_since(self.env, "0" * 40))
        with mock.patch("agent_run.subprocess.run", side_effect=subprocess.TimeoutExpired("git", 30)):
            self.assertIsNone(agent_run.commits_since(self.env, self.start))


class Steps:
    def __init__(self, n):
        self.steps, self.ticks = n, 0

    def __call__(self):
        self.ticks += 1


class StallWatchTest(unittest.TestCase):
    def watch(self, steps, commits, state=None):
        state = state or lp.State(start="s")
        path = os.path.join(tempfile.mkdtemp(), "loop.json")
        with mock.patch("agent_run.commits_since", return_value=commits) as since:
            tick = agent_run.stall_watch({}, steps, POLICY, state, path)
            try:
                tick()
            finally:
                self.since = since
        return state, path

    def test_below_the_limit_git_is_not_asked(self):
        steps = Steps(79)
        self.watch(steps, 0)
        self.assertEqual(steps.ticks, 1, "the step log still runs")
        self.since.assert_not_called()

    def test_no_commit_by_the_limit_is_a_stall(self):
        with self.assertRaises(agent_run.Stalled):
            self.watch(Steps(80), 0)

    def test_a_commit_spends_the_rule_for_the_rest_of_the_run(self):
        state, path = self.watch(Steps(80), 1)
        self.assertTrue(state.committed)
        self.assertTrue(lp.State.load(path, "x").committed)
        self.watch(Steps(400), 0, state)
        self.since.assert_not_called()

    def test_git_that_cannot_tell_is_never_a_stall(self):
        state, _ = self.watch(Steps(80), None)
        self.assertFalse(state.committed)


if __name__ == "__main__":
    unittest.main()
