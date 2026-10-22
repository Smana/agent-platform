"""agent_run's half of the loop policy (loops design L2, L3): the commit count and the stall rule on
a real clone, the checks and the judge's call against stand-ins, the drive, and the end's report.
Needs the openhands SDK for the message shape, so it runs in the image: docker build --target test.
"""
import http.client
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


class CheckRunsTest(unittest.TestCase):
    def gh(self, script):
        """A gh on PATH that records its arguments and runs script."""
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        with open(os.path.join(self.dir, "gh"), "w") as f:
            f.write('#!/bin/sh\necho "$*" > "$(dirname "$0")/args"\n' + script)
        os.chmod(os.path.join(self.dir, "gh"), 0o755)
        mock.patch.dict(os.environ, {"PATH": self.dir + os.pathsep + os.environ["PATH"]}).start()
        self.addCleanup(mock.patch.stopall)

    def test_reads_name_status_conclusion_and_title(self):
        api = {"check_runs": [{"name": "check", "status": "completed", "conclusion": "failure",
                               "output": {"title": "2 errors", "summary": "long"}}]}
        self.gh("echo '%s'" % json.dumps(api))
        self.assertEqual(agent_run.check_runs({"REPOSITORY": "o/r"}, "abc"),
                         [{"name": "check", "status": "completed", "conclusion": "failure", "title": "2 errors"}])
        with open(os.path.join(self.dir, "args")) as f:
            self.assertEqual(f.read().strip(), "api repos/o/r/commits/abc/check-runs?per_page=100")

    def test_an_unknown_head_is_none(self):
        self.gh("echo 'No commit found for SHA' >&2; exit 1")
        self.assertIsNone(agent_run.check_runs({"REPOSITORY": "o/r"}, "abc"))

    def test_waits_for_pending_checks_then_stops_waiting(self):
        clock, slept = [0.0], []
        pending, done = [dict(RUNS[1])], [dict(RUNS[1], status="completed", conclusion="success")]

        def sleep(s):
            slept.append(s)
            clock[0] += s

        for answers, want, waited in (([pending, done], done, [30]),
                                       ([pending] * 20, pending, [30] * 10),
                                       ([[]] * 20, [], [30] * 2)):
            with self.subTest(want=want, waited=len(waited)):
                clock[0], slept[:] = 0.0, []
                with mock.patch("agent_run.check_runs", side_effect=answers):
                    self.assertEqual(agent_run.wait_checks({}, "abc", clock=lambda: clock[0], sleep=sleep), want)
                self.assertEqual(slept, waited)


class EvidenceTest(Clone, unittest.TestCase):
    def test_the_diff_against_the_base_and_the_heads_checks(self):
        self.commit(self.repo, "docs.md")
        sha = agent_run.head()
        with mock.patch("agent_run.wait_checks", return_value=RUNS[:1]) as checks:
            text, offered = agent_run.evidence(self.env, BRIEF)
        checks.assert_called_once_with(self.env, sha)
        self.assertEqual(offered, {"acceptance", "diff", "check:check", "file:docs.md"})
        self.assertIn("`task docs-links` passes.", text)
        self.assertIn("+docs.md", text)
        self.assertIn("CHECKS' OUTPUT on " + sha, text)


class FakeLLM(http.server.BaseHTTPRequestHandler):
    """The identity proxy's chat completions route as the judge's call reaches it."""
    requests, status, content = [], 200, ""

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        FakeLLM.requests.append((self.path, dict(self.headers), body))
        out = json.dumps({"model": "glm-5.3-flash", "choices": [{"message": {"role": "assistant", "content": FakeLLM.content}}]}).encode()
        self.send_response(FakeLLM.status)
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass


class FakeProxy:
    def setUp(self):
        FakeLLM.requests, FakeLLM.status, FakeLLM.content = [], 200, '{"verdict": "met"}'
        server = http.server.HTTPServer(("127.0.0.1", 0), FakeLLM)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = "http://127.0.0.1:%d" % server.server_port


class AskJudgeTest(FakeProxy, unittest.TestCase):
    def test_a_fresh_context_with_no_tools_on_the_proxys_judge_path(self):
        answer = agent_run.ask_judge({"LLM_BASE_URL": self.url + "/v1"}, "tier-light", "evidence")
        self.assertEqual(answer, ('{"verdict": "met"}', "glm-5.3-flash"))
        [(path, headers, body)] = FakeLLM.requests
        self.assertEqual(path, "/judge/v1/chat/completions", "the proxy pins the judge's alias there (P4)")
        self.assertEqual(headers["Authorization"], "Bearer " + agent_run.PLACEHOLDER_KEY)
        self.assertEqual(body["model"], "tier-light")
        self.assertEqual([m["role"] for m in body["messages"]], ["system", "user"])
        self.assertEqual(body["messages"][1]["content"], "evidence")
        self.assertEqual(body["reasoning_effort"], "low")
        self.assertNotIn("tools", body)
        self.assertNotIn("temperature", body)

    def test_a_refused_or_missing_judge_path_is_no_answer(self):
        # 403: JUDGE_MODEL none, or outside the profile. 404: a proxy before P4. 429: the run's budget.
        for status in (403, 404, 429):
            with self.subTest(status=status):
                FakeLLM.status = status
                with mock.patch("sys.stderr", new=io.StringIO()) as err:
                    self.assertIsNone(agent_run.ask_judge({"LLM_BASE_URL": self.url + "/v1/"}, "tier-light", "e"))
                self.assertIn("agent-run judge unavailable: HTTP Error %d" % status, err.getvalue())

    def test_no_proxy_or_no_v1_is_no_answer(self):
        with mock.patch("sys.stderr", new=io.StringIO()):
            self.assertIsNone(agent_run.ask_judge({"LLM_BASE_URL": "http://127.0.0.1:1/v1"}, "tier-light", "e"))
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            self.assertIsNone(agent_run.ask_judge({"LLM_BASE_URL": self.url + "/api"}, "tier-light", "e"))
        self.assertIn("has no /v1", err.getvalue())
        self.assertEqual(FakeLLM.requests, [], "nothing is sent beside /api")


class AskJudgeFailuresTest(FakeProxy, unittest.TestCase):
    def ask(self, env):
        with mock.patch("sys.stderr", new=io.StringIO()) as err:
            return agent_run.ask_judge(env, "tier-light", "e"), err.getvalue()

    def test_a_truncated_response_is_no_answer(self):
        with mock.patch("http.client.HTTPResponse.read", side_effect=http.client.IncompleteRead(b"x")):
            answer, err = self.ask({"LLM_BASE_URL": self.url + "/v1"})
        self.assertIsNone(answer)
        self.assertIn("judge unavailable", err)

    def test_an_unreadable_key_file_is_no_answer(self):
        answer, err = self.ask({"LLM_BASE_URL": self.url + "/v1", "LLM_API_KEY_FILE": "/nonexistent/key"})
        self.assertIsNone(answer)
        self.assertIn("judge unavailable", err)
        self.assertEqual(FakeLLM.requests, [])

    def test_a_redirect_is_no_answer_and_is_not_followed(self):
        FakeLLM.status = 302
        answer, err = self.ask({"LLM_BASE_URL": self.url + "/v1"})
        self.assertIsNone(answer)
        self.assertIn("HTTP Error 302", err)
        self.assertEqual(len(FakeLLM.requests), 1)

    def test_a_host_only_base_is_no_answer(self):
        for base in ("http://v1", "http://127.0.0.1:1/api?x=/v1"):
            with self.subTest(base=base):
                answer, _ = self.ask({"LLM_BASE_URL": base})
                self.assertIsNone(answer)
        self.assertEqual(FakeLLM.requests, [])


class ContinueTest(unittest.TestCase):
    def test_the_message_is_a_send_message_request_the_sdk_accepts(self):
        from openhands.agent_server.models import SendMessageRequest
        calls = []
        with mock.patch("agent_run.http", side_effect=lambda *a, **k: calls.append(a) or {}):
            agent_run.continue_conversation("cid", "go on")
        method, path, body = calls[0]
        self.assertEqual((method, path), ("POST", "/api/conversations/cid/events"))
        req = SendMessageRequest.model_validate(body)
        self.assertTrue(req.run)
        self.assertEqual(req.content[0].text, "go on")


if __name__ == "__main__":
    unittest.main()
