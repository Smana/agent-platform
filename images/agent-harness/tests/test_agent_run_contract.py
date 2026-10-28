"""The test-first tester in agent-run (v3 feature 4, F1): the tester's red check, signed push and ends,
the implementer's contract, hook and hold. Offline proofs 2 and 3 on real repositories and a local bare
remote; the conversation is scripted as in test_loop_run.DriveTest."""
import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import agent_run  # noqa: E402
import loop_policy as lp  # noqa: E402
import test_contract as tc  # noqa: E402
import test_loop_run as tlr  # noqa: E402
from test_test_contract import RUN_SH, SHELL_DECL  # noqa: E402

REAL_PUSH_URL = agent_run.push_url
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
        mock.patch.object(agent_run, "push_url", return_value=self.origin).start()  # the forge is the local bare origin
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
        _, res = agent_run.red_check(dict(os.environ), self.base, dict(os.environ))
        res = {**res, "files": ["t/test-name.sh"], "ids": ["t/a\nb: x", "t/c"]}
        sha = agent_run.sign(self.env, self.base, res)
        msg = git(self.repo, "show", "-s", "--format=%B", sha)
        parsed = subprocess.run(["git", "interpret-trailers", "--parse"], input=msg, capture_output=True, text=True).stdout.splitlines()
        ids = [t.split(": ", 1)[1] for t in parsed if t.startswith("Agent-Contract-Id")]
        self.assertEqual(ids, ["t/a b: x", "t/c"])


class Notes:
    def __init__(self):
        self.finals = []

    def final(self, text):
        self.finals.append(text)

    def judge(self, *args):
        pass


class FakeSpan:
    def __init__(self, name):
        self.name, self.attrs, self.ended = name, {}, False

    def set_attribute(self, k, v):
        self.attrs[k] = v

    def set_status(self, s):
        pass

    def end(self):
        self.ended = True


class TesterEndTest(Origin, unittest.TestCase):
    RED = RedCheckTest.RED
    VACUOUS = {"t/test-vacuous.sh": "exit 0\n"}

    def setUp(self):
        super().setUp()
        try:
            import opentelemetry.trace  # noqa: F401  (the image has it)
        except ImportError:
            trace = types.ModuleType("opentelemetry.trace")
            trace.Status, trace.StatusCode = (lambda *a, **k: None), types.SimpleNamespace(ERROR="ERROR")
            mock.patch.dict(sys.modules, {"opentelemetry": types.ModuleType("opentelemetry"), "opentelemetry.trace": trace}).start()

    def end(self, passes, files=None, cont_log=None):
        """tester_end after a finished conversation: passes[0] is the first pass (already done by the
        caller's commit), each later one runs when a continuation is sent."""
        self.sent, self.spans = [], []
        passes = list(passes)
        notes = Notes()

        def cont(cid, text):
            self.sent.append(text)
            passes.pop(0)()

        def start(name):
            self.spans.append(FakeSpan(name))
            return self.spans[-1]

        with mock.patch("agent_run.poll", return_value=0), mock.patch("agent_run.continue_conversation", side_effect=cont), \
                mock.patch("sys.stdout", new=io.StringIO()):
            end = agent_run.tester_end(self.env, "cid", self.base, dict(os.environ), notes, start)
        self.notes = notes.finals
        return end

    def pushed(self):
        return bool(git(self.origin, "branch", "--list", "agent/x"))

    def test_red_first_time_is_pushed_and_ready(self):
        self.commit(self.RED)
        self.assertEqual(self.end([]), "contract_ready")
        self.assertEqual(self.sent, [])
        self.assertTrue(self.pushed())
        self.assertEqual(self.notes, ["contract_ready: t/test-name.sh"])

    def test_a_vacuous_test_gets_one_continuation_then_not_red(self):
        self.commit(self.VACUOUS)
        self.assertEqual(self.end([lambda: self.commit({"t/test-vacuous2.sh": "exit 0\n"}, "again")]), "contract_not_red")
        self.assertEqual(len(self.sent), 1)
        fence = [w for w in self.sent[0].split() if w.startswith("CONTRACT-")][0]
        self.assertEqual(self.sent[0].count("\n" + fence + "\n"), 2, "the fence opens and closes the data")
        between = self.sent[0].split("\n" + fence + "\n")[1]
        self.assertIn("t/test-vacuous.sh", between)
        self.assertFalse(self.pushed())

    def test_a_continuation_that_fixes_it_is_ready(self):
        self.commit(self.VACUOUS)
        def fix():
            git(self.repo, "rm", "-q", "t/test-vacuous.sh")
            self.commit(self.RED, "fix")

        self.assertEqual(self.end([fix]), "contract_ready")
        self.assertEqual(len(self.sent), 1)
        self.assertTrue(self.pushed())

    def test_outside_the_paths_continues_once_then_ends_outside(self):
        self.commit({**self.RED, "bin/greet": "#!/usr/bin/env bash\necho x\n"})
        self.assertEqual(self.end([lambda: self.commit({"README": "x\n"}, "still")]), "contract_outside_tests")
        self.assertEqual(len(self.sent), 1)
        self.assertFalse(self.pushed())

    def test_could_not_run_never_continues(self):
        decl = SHELL_DECL.replace('run: t/run.sh "$@"', "run: exit 0")
        self.write(self.repo, {".github/agent-checks.yaml": decl})
        git(self.repo, "add", "-A")
        git(self.repo, "-c", "core.hooksPath=/dev/null", "commit", "-qm", "decl")
        git(self.repo, "push", "-q", "origin", "HEAD:main")
        git(self.repo, "fetch", "-q", "origin")
        base = git(self.repo, "rev-parse", "origin/main")
        self.base = base
        self.commit(self.RED)
        self.assertEqual(self.end([]), "contract_not_red")
        self.assertEqual(self.sent, [])
        self.assertIn("the runner could not run", self.notes[0])

    def test_no_commit_is_not_testable(self):
        self.assertEqual(self.end([]), "not_testable")
        self.assertEqual(self.sent, [])

    def test_no_declaration_is_not_red_without_a_continuation(self):
        git(self.repo, "rm", "-q", ".github/agent-checks.yaml")
        git(self.repo, "-c", "core.hooksPath=/dev/null", "commit", "-qm", "drop")
        git(self.repo, "push", "-q", "origin", "HEAD:main")
        git(self.repo, "fetch", "-q", "origin")
        self.base = git(self.repo, "rev-parse", "origin/main")
        self.commit(self.RED)
        self.assertEqual(self.end([]), "contract_not_red")
        self.assertEqual(self.sent, [])
        self.assertIn("declares no tests", self.notes[0])
        self.assertFalse(self.pushed())

    def test_a_refused_push_is_not_red_and_pushes_nothing(self):
        self.commit(self.RED)
        with mock.patch("agent_run.push_contract", return_value="remote: permission denied"):
            self.assertEqual(self.end([]), "contract_not_red")
        self.assertEqual(self.sent, [])
        self.assertIn("the push was refused", self.notes[0])
        self.assertFalse(self.pushed())

    def test_the_note_never_exceeds_280_characters(self):
        files = ["t/test-%s-%02d.sh" % ("x" * 30, i) for i in range(60)]
        note = agent_run.contract_note("contract_ready", {"files": files})
        self.assertLessEqual(len(agent_run._short("Harness (tester): " + note, 280)), 280)
        why = agent_run.contract_note("contract_not_red", {"why": "w" * 900})
        self.assertLessEqual(len(agent_run._short("Harness (tester): " + why, 280)), 280)

    def test_every_end_sets_the_span_error_but_ready(self):
        self.commit(self.RED)
        self.end([])
        span = self.spans[0]
        self.assertEqual(span.name, agent_run.CONTRACT_SPAN)
        self.assertTrue(span.ended)
        self.assertNotIn("error.type", span.attrs)

    def test_a_not_ready_end_names_itself_on_the_span(self):
        self.assertEqual(self.end([]), "not_testable")
        self.assertEqual(self.spans[0].attrs["error.type"], "not_testable")

    def test_a_poll_that_does_not_finish_after_the_continuation_stands_as_checked(self):
        self.commit(self.VACUOUS)
        self.sent = []
        with mock.patch("agent_run.poll", return_value=1), \
                mock.patch("agent_run.continue_conversation", side_effect=lambda c, t: self.sent.append(t)), \
                mock.patch("sys.stdout", new=io.StringIO()):
            end = agent_run.tester_end(self.env, "cid", self.base, dict(os.environ))
        self.assertEqual(end, "contract_not_red")
        self.assertEqual(len(self.sent), 1)


class Contracted(Origin):
    """The origin's agent/x holds a signed contract commit; the clone is on it, as a resumed implementer's."""
    RED = RedCheckTest.RED

    def setUp(self):
        super().setUp()
        self.commit(self.RED)
        _, res = agent_run.red_check(dict(os.environ), self.base, dict(os.environ))
        self.signed = agent_run.sign(self.env, self.base, res)
        self.assertIsNone(agent_run.push_contract(self.env, self.signed))
        git(self.repo, "fetch", "-q", "origin")
        git(self.repo, "checkout", "-q", "-B", "agent/x", "origin/agent/x")
        self.env = {**self.env, "ROLE": "implementer"}


class ImplementerContractTest(Contracted, unittest.TestCase):
    def test_a_resumed_branch_with_a_contract_writes_the_file(self):
        c = agent_run.load_contract(self.env, True, self.base)
        self.assertEqual(c, {"commit": self.signed, "files": ["t/test-name.sh"], "ids": ["t/test-name.sh"], "base": self.base})
        with open(agent_run.CONTRACT_FILE) as f:
            self.assertEqual(json.load(f), c)
        self.assertEqual(os.stat(agent_run.CONTRACT_FILE).st_mode & 0o777, 0o644)

    def test_no_contract_or_a_fresh_branch_writes_nothing(self):
        self.assertIsNone(agent_run.load_contract(self.env, False, self.base))
        self.assertFalse(os.path.exists(agent_run.CONTRACT_FILE))
        git(self.repo, "checkout", "-q", "-B", "agent/y", "origin/main")
        self.assertIsNone(agent_run.load_contract(self.env, True, self.base))
        self.assertFalse(os.path.exists(agent_run.CONTRACT_FILE))
        self.assertIsNone(agent_run.load_contract({**self.env, "ROLE": "reviewer"}, True, self.base))

    def test_the_rules_name_the_files_ids_and_the_hold(self):
        c = agent_run.load_contract(self.env, True, self.base)
        rules = agent_run.contract_rules(self.env, c, None)
        for word in ("t/test-name.sh", "may not change", "test-contract hold", self.signed[:12]):
            self.assertIn(word, rules)

    def test_the_testers_rules_name_the_paths_and_never_the_push(self):
        tests = {"paths": ["t/"], "select": ["t/test-*.sh"]}
        rules = agent_run.contract_rules({"ROLE": "tester"}, None, tests)
        for word in ("t/", "t/test-*.sh", "never push", "test-contract red"):
            self.assertIn(word, rules)
        self.assertEqual(agent_run.contract_rules({"ROLE": "tester"}, None, None), "")
        self.assertEqual(agent_run.contract_rules({"ROLE": "implementer"}, None, tests), "")


HOOK = next(p for p in (os.path.join(HERE, "pre-commit"), "/etc/agent/git-hooks/pre-commit") if os.path.exists(p))


class HookTest(Contracted, unittest.TestCase):
    def setUp(self):
        super().setUp()
        self.hooks = os.path.join(self.d, "hooks")
        os.makedirs(self.hooks)
        with open(os.path.join(self.hooks, "pre-commit"), "w") as f:  # the image's shebang is not on this machine
            f.write("#!/bin/sh\nexec %s %s\n" % (sys.executable, HOOK))
        os.chmod(os.path.join(self.hooks, "pre-commit"), 0o755)
        self.contract = agent_run.load_contract(self.env, True, self.base)
        mock.patch.dict(os.environ, {"AGENT_CONTRACT_FILE": agent_run.CONTRACT_FILE}).start()

    def hook(self):
        return subprocess.run([sys.executable, HOOK], cwd=self.repo, capture_output=True, text=True)

    def stage(self, files):
        self.write(self.repo, files)
        git(self.repo, "add", "-A")

    def test_a_staged_contract_file_is_refused_and_named(self):
        self.stage({"t/test-name.sh": "exit 0\n"})
        p = self.hook()
        self.assertEqual(p.returncode, 1)
        self.assertIn("t/test-name.sh", p.stderr)
        self.assertIn("git restore --staged --worktree --source=%s" % self.signed, p.stderr)

    def test_another_file_passes(self):
        self.stage({"bin/greet": "#!/usr/bin/env bash\necho hi\n"})
        self.assertEqual(self.hook().returncode, 0)

    def test_no_contract_file_passes(self):
        os.remove(agent_run.CONTRACT_FILE)
        self.stage({"t/test-name.sh": "exit 0\n"})
        self.assertEqual(self.hook().returncode, 0)

    def test_an_unreadable_contract_file_passes(self):
        with open(agent_run.CONTRACT_FILE, "w") as f:
            f.write("not json")
        self.stage({"t/test-name.sh": "exit 0\n"})
        self.assertEqual(self.hook().returncode, 0)

    def test_a_real_commit_is_refused_and_no_verify_passes_it(self):
        self.stage({"t/test-name.sh": "exit 0\n"})
        p = subprocess.run(["git", "-C", self.repo, "-c", "core.hooksPath=" + self.hooks, "commit", "-qm", "x"], capture_output=True, text=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("test contract", p.stderr)
        subprocess.run(["git", "-C", self.repo, "-c", "core.hooksPath=" + self.hooks, "commit", "-q", "--no-verify", "-m", "x"], check=True)

    def test_a_checkpoint_leaves_the_contract_files_out_and_still_pushes(self):
        git(self.repo, "config", "core.hooksPath", self.hooks)
        self.commit({"bin/mine": "x\n"}, "agent work")
        self.write(self.repo, {"t/test-name.sh": "exit 0\n", "bin/greet": "#!/usr/bin/env bash\necho dirty\n"})
        result = agent_run.checkpoint(self.env, time.monotonic() + 60)
        self.assertIn("pushed", result)
        status = git(self.repo, "status", "--porcelain")
        self.assertEqual(status, "M t/test-name.sh", "modified in the work tree, not staged")
        self.assertEqual(git(self.origin, "show", "agent/x:bin/greet"), "#!/usr/bin/env bash\necho dirty")
        self.assertEqual(git(self.origin, "show", "agent/x:bin/mine"), "x")
        self.assertEqual(git(self.origin, "show", "agent/x:t/test-name.sh"), self.RED["t/test-name.sh"].strip())


class DriveContractTest(unittest.TestCase):
    """drive() with a scripted hold: each finish asks the hold, a red one continues before any judge."""
    POLICY = lp.Policy(stall_steps=80, continuations=2, judge_model="tier-light")
    MET = (json.dumps({"verdict": "met", "reason": "r", "evidence": ["diff"]}), "m")

    def drive(self, holds, answers=(), policy=POLICY, polls=None):
        self.sent, self.judged, self.notes = [], [], Notes()
        holds = iter(holds)
        polls = iter(polls or [0] * 10)

        def judge(env, model, message):
            self.judged.append(message)
            return next(answers_it)

        answers_it = iter(answers)
        state = lp.State(start="s")
        with mock.patch("agent_run.poll", side_effect=lambda cid, on_tick=None: next(polls)), \
                mock.patch("agent_run.evidence", return_value=("e", {"diff"})), mock.patch("agent_run.ask_judge", side_effect=judge), \
                mock.patch("agent_run.commits_since", return_value=1), mock.patch("agent_run.head", return_value="h"), \
                mock.patch("agent_run.continue_conversation", side_effect=lambda cid, text: self.sent.append(text)), \
                mock.patch("sys.stdout", new=io.StringIO()):
            result = agent_run.drive({}, "cid", tlr.Steps(0), policy, state, "task", os.path.join(tempfile.mkdtemp(), "l.json"),
                                     self.notes, hold=lambda: next(holds))
        return result, state

    RED = {"result": "red", "failing": ["t/a.sh"]}
    GREEN = {"result": "green", "failing": []}

    def test_a_red_contract_continues_without_asking_the_judge(self):
        result, state = self.drive([self.RED, self.GREEN], [self.MET])
        self.assertEqual(result, (0, None, ""))
        self.assertEqual(len(self.judged), 1, "the judge is asked once, after the second finish")
        self.assertEqual(len(self.sent), 1)
        self.assertIn("the test contract", self.sent[0])
        self.assertIn("t/a.sh", self.sent[0])
        self.assertEqual(state.sent, 1)

    def test_a_green_contract_asks_the_judge_as_today(self):
        result, _ = self.drive([self.GREEN], [self.MET])
        self.assertEqual(result, (0, None, ""))
        self.assertEqual((len(self.judged), self.sent), (1, []))

    def test_a_modified_contract_or_could_not_run_never_continues(self):
        for res, text in (({"result": "contract_modified", "failing": []}, "contract contract_modified"),
                          ({"result": "could_not_run"}, "contract could_not_run")):
            result, _ = self.drive([res], [self.MET])
            self.assertEqual((result, self.sent, len(self.judged)), ((0, None, ""), [], 1), res)
            self.assertEqual(self.notes.finals, [text])

    def test_spent_continuations_end_not_met_under_the_loop(self):
        result, _ = self.drive([self.RED] * 3, [])
        self.assertEqual(result[:2], (1, "not_met"))
        self.assertIn("t/a.sh", result[2])
        self.assertEqual(len(self.sent), 2)
        self.assertEqual(self.judged, [])

    def test_an_error_ends_without_asking_the_hold(self):
        result, _ = self.drive([], [], polls=[1])
        self.assertEqual(result, (1, None, ""))

    def test_a_loop_without_a_judge_still_holds_the_contract(self):
        result, _ = self.drive([self.RED, self.GREEN], [], policy=lp.Policy(80, 2, None))
        self.assertEqual((result, len(self.sent), self.judged), ((0, None, ""), 1, []))


class ContractLoopTest(unittest.TestCase):
    def loop(self, holds, polls=None):
        self.sent = []
        holds = iter(holds)
        polls = iter(polls or [0] * 10)
        steps = tlr.Steps(10000)
        with mock.patch("agent_run.poll", side_effect=lambda cid, on_tick=None: next(polls)), \
                mock.patch("agent_run.commits_since", return_value=1), mock.patch("agent_run.head", return_value="h"), \
                mock.patch("agent_run.ask_judge") as judge, \
                mock.patch("agent_run.continue_conversation", side_effect=lambda cid, text: self.sent.append(text)), \
                mock.patch("sys.stdout", new=io.StringIO()):
            code = agent_run.contract_loop({}, "cid", steps, lambda: next(holds), Notes())
        self.assertFalse(judge.called, "no judge in the contract-only loop")
        return code

    RED = DriveContractTest.RED
    GREEN = DriveContractTest.GREEN

    def test_red_twice_then_green_is_two_continuations_and_exit_0(self):
        self.assertEqual(self.loop([self.RED, self.RED, self.GREEN]), 0)
        self.assertEqual(len(self.sent), 2)

    def test_red_three_times_spends_the_continuations_and_still_exits_0(self):
        self.assertEqual(self.loop([self.RED] * 3), 0)
        self.assertEqual(len(self.sent), 2)

    def test_there_is_no_stall_rule(self):
        self.assertEqual(self.loop([self.GREEN]), 0)

    def test_a_failed_conversation_exits_as_it_did(self):
        self.assertEqual(self.loop([], polls=[1]), 1)


class MainContractTest(unittest.TestCase):
    """main()'s wiring, MainLoopTest's patching: the tester's mode, the implementer's contract."""
    DECL = {"tools": [], "minutes": 5, "pin": ("b", "blob")}
    CONTRACT = {"commit": "c0ffee", "files": ["t/a.sh"], "ids": ["t/a.sh"], "base": "b"}

    def main(self, env_extra, poll=0, resumed=False, decl=DECL, contract=None, tester_end=None):
        mocks = {}
        with tempfile.TemporaryDirectory() as d, contextlib.ExitStack() as stack:
            task = os.path.join(d, "task.md")
            with open(task, "w") as f:
                f.write("task")
            env = {"TASK_FILE": task, "RULES_FILE": task, "ROLE": "tester", **env_extra}
            stack.enter_context(mock.patch.dict(os.environ, env, clear=True))
            stack.enter_context(mock.patch("agent_run.LOOP_STATE", os.path.join(d, "loop.json")))
            for name, kw in {"subprocess.Popen": {}, "wait_ready": {}, "persisted": {"return_value": resumed},
                             "clone": {"return_value": False}, "declared": {"return_value": decl},
                             "prefetch": {"return_value": "proc"}, "start_base": {"return_value": "b"},
                             "load_contract": {"return_value": contract}, "build_request": {"return_value": {}},
                             "head": {"return_value": "h0"}, "http": {"return_value": {"id": "c1"}},
                             "poll": {"return_value": poll}, "drive": {"return_value": (0, None, "")},
                             "contract_loop": {"return_value": 0}, "make_hold": {"return_value": "hold"},
                             "disable_push": {}, "tester_end": tester_end or {"return_value": "contract_not_red"},
                             "contract_env": {"return_value": {}}, "final_checks": {}, "checks_ledger": {"return_value": ""},
                             "StepLog": {}, "ProgressNotes": {}, "final_read": {}, "stop": {"return_value": "stopped"},
                             "revoke": {}}.items():
                mocks[name] = stack.enter_context(mock.patch("agent_run." + name, **kw))
            stack.enter_context(mock.patch("sys.stdout", new=io.StringIO()))
            stack.enter_context(mock.patch("sys.stderr", new=io.StringIO()))
            code = agent_run.main()
        return types.SimpleNamespace(code=code, **{k.replace(".", "_"): v for k, v in mocks.items()})

    def test_a_tester_without_a_task_url_commits_only_and_always_exits_0(self):
        r = self.main({})
        self.assertEqual(r.code, 0)
        r.disable_push.assert_called_once()
        r.tester_end.assert_called_once()
        self.assertEqual(r.tester_end.call_args.args[:3], (mock.ANY, "c1", "b"))
        r.prefetch.assert_called_once()

    def test_a_tester_with_a_task_url_is_todays_verifier(self):
        r = self.main({"TASK_URL": "https://github.com/o/r/pull/1"})
        self.assertEqual(r.code, 0)
        r.tester_end.assert_not_called()
        r.contract_env.assert_not_called()

    def test_a_failed_tester_conversation_is_not_checked(self):
        r = self.main({}, poll=1)
        self.assertEqual(r.code, 1)
        r.tester_end.assert_not_called()

    def test_a_tester_end_that_raises_still_exits_0(self):
        r = self.main({}, tester_end={"side_effect": RuntimeError("boom")})
        self.assertEqual(r.code, 0)

    def test_a_resumed_implementer_with_a_contract_holds_it_without_a_loop(self):
        r = self.main({"ROLE": "implementer"}, resumed=True, contract=self.CONTRACT)
        r.make_hold.assert_called_once()
        r.contract_loop.assert_called_once()
        r.poll.assert_not_called()
        r.drive.assert_not_called()
        r.tester_end.assert_not_called()

    def test_with_a_loop_drive_gets_the_hold(self):
        r = self.main({"ROLE": "implementer", "AGENT_LOOP": "{}"}, resumed=True, contract=self.CONTRACT)
        r.drive.assert_called_once()
        self.assertEqual(r.drive.call_args.kwargs["hold"], "hold")
        r.contract_loop.assert_not_called()

    def test_without_a_contract_it_is_todays_run(self):
        r = self.main({"ROLE": "implementer"}, resumed=True)
        r.make_hold.assert_not_called()
        r.contract_loop.assert_not_called()
        r.drive.assert_not_called()
        r.poll.assert_called_once()
        r.tester_end.assert_not_called()

    def test_a_loop_without_a_contract_gets_no_hold(self):
        r = self.main({"ROLE": "implementer", "AGENT_LOOP": "{}"})
        self.assertIsNone(r.drive.call_args.kwargs["hold"])


class ReviewFixesTest(Origin, unittest.TestCase):
    RED = RedCheckTest.RED

    def test_sign_signs_the_checked_object_not_a_later_head(self):
        self.commit(self.RED)
        _, res = agent_run.red_check(dict(os.environ), self.base, dict(os.environ))
        self.commit({"t/test-late.sh": "exit 1\n"}, "committed by a background process during the check")
        sha = agent_run.sign(self.env, self.base, res)
        self.assertEqual(git(self.repo, "rev-parse", sha + "^"), self.base)
        self.assertEqual(git(self.repo, "rev-parse", sha + "^{tree}"), git(self.repo, "rev-parse", res["commit"] + "^{tree}"))
        self.assertNotIn("t/test-late.sh", git(self.repo, "ls-tree", "-r", "--name-only", sha))

    def test_sign_refuses_a_checked_commit_not_on_the_base(self):
        self.commit(self.RED)
        _, res = agent_run.red_check(dict(os.environ), self.base, dict(os.environ))
        other = git(self.repo, "commit-tree", "HEAD^{tree}", "-m", "x")
        with self.assertRaises(RuntimeError):
            agent_run.sign(self.env, self.base, {**res, "commit": other})

    def test_the_push_ignores_the_clones_remote_and_rewrites(self):
        self.commit(self.RED)
        _, res = agent_run.red_check(dict(os.environ), self.base, dict(os.environ))
        sha = agent_run.sign(self.env, self.base, res)
        evil = os.path.join(self.d, "evil.git")
        subprocess.run(["git", "init", "-q", "--bare", evil], check=True)
        git(self.repo, "config", "remote.origin.url", evil)
        git(self.repo, "config", "url.%s.insteadOf" % evil, self.origin)
        git(self.repo, "config", "url.%s.insteadOf" % evil, "https://github.com/o/r.git")
        self.assertEqual(REAL_PUSH_URL(self.env), "https://github.com/o/r.git")
        self.assertIsNone(agent_run.push_contract(self.env, sha))
        self.assertEqual(git(self.origin, "rev-parse", "agent/x"), sha)
        self.assertEqual(git(evil, "branch", "--list"), "")

    def test_the_continuation_caps_each_item(self):
        res = {"why": "w" * 5000, "rows": [{"id": "i" * 5000, "base": "-", "overlay": "pass"}], "outside": ["o" * 5000]}
        text = agent_run.tester_continuation(res, ["t/"], "b", "F")
        self.assertLess(len(text), 1500)

    def test_a_red_check_that_raises_still_posts_an_end(self):
        notes = Notes()
        with mock.patch("agent_run.red_check", side_effect=RuntimeError("boom")), mock.patch("sys.stdout", new=io.StringIO()):
            end = agent_run.tester_end(self.env, "cid", self.base, {}, notes)
        self.assertEqual(end, "contract_not_red")
        self.assertEqual(len(notes.finals), 1)
        self.assertIn("could not run", notes.finals[0])
