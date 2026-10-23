"""test-contract (v3 feature 4, offline proof 1): the red check, the CI's judgement and the implementer's
hold, on two fixture repositories: one pytest, one of shell suites whose runner writes one testcase a file.

Each case builds an origin whose main declares the tests, clones it, and calls test_contract in-process.
"""
import importlib.util
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import yaml

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import test_contract as tc  # noqa: E402

PYTEST_DECL = """\
version: 1
steps: [{name: tests, run: python3 -m pytest -q, ci: test}]
tests:
  paths: [tests/]
  select: ["tests/test_*.py"]
  runner: [tests/conftest.py]
  run: python3 -m pytest -q -p no:cacheprovider --junitxml="$AGENT_TEST_REPORT" "$@"
"""

# The shell runner: the host's run.sh contract in small. Files given: those only. One testcase a file;
# exit 77 is a skip.
RUN_SH = r"""#!/usr/bin/env bash
set -uo pipefail
report="${AGENT_TEST_REPORT:-}"
cases=""
for t in "$@"; do
  out="$(bash "$t" 2>&1)"; code=$?
  case "$code" in
    0) body="" ;;
    77) body="<skipped/>" ;;
    *) body="<failure/>" ;;
  esac
  cases="$cases<testcase classname=\"$t\" name=\"$t\">$body</testcase>"
done
[ -n "$report" ] && printf '<testsuite>%s</testsuite>\n' "$cases" > "$report"
exit 0
"""

SHELL_DECL = """\
version: 1
steps: [{name: suites, run: t/run.sh t/test-*.sh, ci: test}]
tests:
  paths: [t/]
  select: ["t/test-*.sh"]
  runner: [t/run.sh]
  run: t/run.sh "$@"
"""


def sh(cwd, *args):
    # The image's /etc/gitconfig hooks every commit (commit-msg needs RUN_ID): fixtures read none of it.
    subprocess.run(args, cwd=cwd, check=True, capture_output=True, env={**os.environ, "GIT_CONFIG_NOSYSTEM": "1"})


class Fixture:
    decl = PYTEST_DECL
    files = {"src/calc.py": "def add(a, b):\n    return a + b\n",
             "tests/test_calc.py": "from src.calc import add\n\ndef test_add():\n    assert add(1, 2) == 3\n",
             "tests/conftest.py": "import sys, os\nsys.path.insert(0, os.path.dirname(os.path.dirname(__file__)))\n",
             "src/__init__.py": ""}

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        seed = os.path.join(self.tmp, "seed")
        os.makedirs(seed)
        sh(seed, "git", "init", "-q", "-b", "main")
        for k, v in ("user.name", "t"), ("user.email", "t@t"):
            sh(seed, "git", "config", k, v)
        self.write(seed, {".github/agent-checks.yaml": self.decl, **self.files})
        sh(seed, "git", "add", "-A")
        sh(seed, "git", "commit", "-qm", "base")
        sh(self.tmp, "git", "clone", "-q", "--bare", seed, "origin")
        self.repo = os.path.join(self.tmp, "repo")
        sh(self.tmp, "git", "clone", "-q", "origin", self.repo)
        for k, v in ("user.name", "t"), ("user.email", "t@t"):
            sh(self.repo, "git", "config", k, v)
        sh(self.repo, "git", "checkout", "-qb", "agent/task")
        self.base = self.rev("origin/main")

    def write(self, root, files):
        for path, body in files.items():
            full = os.path.join(root, path)
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "w") as f:
                f.write(body)
            if path.endswith(".sh"):
                os.chmod(full, 0o755)

    def commit(self, files, msg="change", trailers=()):
        self.write(self.repo, files)
        sh(self.repo, "git", "add", "-A")
        args = ["git", "commit", "-qm", msg]
        for t in trailers:
            args += ["--trailer", t]
        sh(self.repo, *args)
        return self.rev("HEAD")

    def rev(self, ref):
        return subprocess.run(["git", "-C", self.repo, "rev-parse", ref], capture_output=True, text=True).stdout.strip()

    def red(self):
        return tc.red(self.repo, self.rev("HEAD"), dict(os.environ), lambda m: None)

    def ci(self):
        return tc.ci(self.repo, self.base, self.rev("HEAD"), dict(os.environ), lambda m: None)


CONTRACT = ["Agent-Contract: run-1", "Agent-Contract-File: tests/test_sub.py", "Agent-Contract-Id: tests.test_sub"]
SUB = "tests/test_sub.py"
SUB_TEST = "from src.calc import sub\n\ndef test_sub():\n    assert sub(3, 2) == 1\n"
SUB_IMPL = {"src/calc.py": "def add(a, b):\n    return a + b\n\ndef sub(a, b):\n    return a - b\n"}


def fixture_has_pytest() -> bool:
    """pytest where the fixture's `python3 -m pytest` runs (the PATH's python3), not in this interpreter."""
    return subprocess.run(["python3", "-c", "import pytest"], capture_output=True).returncode == 0


# The image's test stage sets this: there a missing pytest is a failure, never a skip.
REQUIRE_PYTEST = bool(os.environ.get("TEST_CONTRACT_REQUIRE_PYTEST"))


@unittest.skipUnless(REQUIRE_PYTEST or fixture_has_pytest(), "python3 has no pytest (the image's test stage has it)")
class PytestTest(Fixture, unittest.TestCase):
    def test_the_fixture_interpreter_has_pytest(self):
        self.assertTrue(fixture_has_pytest(), "python3 -c 'import pytest' fails: the image's test stage must install it")

    def test_a_new_test_red_at_the_overlay_and_green_at_the_head_is_a_contract_then_green(self):
        self.commit({SUB: SUB_TEST})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OK, "contract_ready"), res)
        self.assertEqual(res["ids"], ["tests.test_sub"])  # the import fails: pytest's collection error, the file's id
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual((code, res["result"]), (tc.OK, "green"), res)

    def test_a_new_test_that_passes_at_the_overlay_is_vacuous(self):
        self.commit({"tests/test_more.py": "from src.calc import add\n\ndef test_more():\n    assert add(2, 2) == 4\n"})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.NOT_OK, "contract_not_red"), res)
        self.assertIn("pass before any change", res["why"])
        self.assertEqual(self.ci()[0], tc.NOT_OK)

    def test_a_skip_at_the_head_is_no_pass(self):
        self.commit({SUB: "import pytest\nfrom src import calc\n\ndef test_sub():\n"
                          "    if not hasattr(calc, 'sub'):\n        assert False\n    pytest.skip('later')\n"})
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual(code, tc.NOT_OK)
        self.assertTrue(any("does not pass at the head (skip)" in w for w in res["why"]), res)

    def test_a_changed_expectation_counts_as_fail_to_pass(self):
        self.commit({"tests/test_calc.py": "from src.calc import add\n\ndef test_add():\n    assert add(1, 2) == 4\n"})
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OK, "contract_ready"), res)
        self.commit({"src/calc.py": "def add(a, b):\n    return a + b + 1\n"})
        self.assertEqual(self.ci()[0], tc.OK)

    def test_no_changed_test_is_green_reporting_none(self):
        self.commit(SUB_IMPL)
        code, res = self.ci()
        self.assertEqual((code, res["why"]), (tc.OK, ["no changed test file"]))

    def test_a_changed_runner_file_is_red(self):
        self.commit({SUB: SUB_TEST, "tests/conftest.py": "import pytest\n"})
        self.assertEqual(self.ci()[1]["result"], "red")
        code, res = self.red()
        self.assertEqual((code, res["result"]), (tc.OUTSIDE, "contract_outside_tests"))

    def test_a_tester_commit_outside_the_test_paths_is_refused(self):
        self.commit({SUB: SUB_TEST, **SUB_IMPL})
        code, res = self.red()
        self.assertEqual((code, res["outside"]), (tc.OUTSIDE, ["src/calc.py"]))

    def test_a_head_that_edits_the_declaration_is_judged_by_the_bases(self):
        self.commit({"tests/test_more.py": "def test_more():\n    assert True\n",
                     ".github/agent-checks.yaml": PYTEST_DECL.replace('"tests/test_*.py"', '"tests/none_*.py"')})
        code, res = self.ci()
        self.assertEqual(code, tc.NOT_OK, res)
        self.assertEqual(res["selected"], ["tests/test_more.py"])

    def test_a_runner_that_could_not_run_is_never_green(self):
        self.commit({SUB: SUB_TEST})
        code, res = tc.red(self.repo, self.rev("HEAD"), {**os.environ, "PATH": "/nonexistent"}, lambda m: None)
        self.assertEqual((code, res["why"]), (tc.COULD_NOT_RUN, "the runner could not run"))
        self.commit(SUB_IMPL)
        code, res = tc.ci(self.repo, self.base, self.rev("HEAD"), {**os.environ, "PATH": "/nonexistent"}, lambda m: None)
        self.assertEqual(code, tc.NOT_OK)
        self.assertIn("could not run", res["why"][0])

    def test_the_implementer_is_held_to_the_contract_commit(self):
        self.commit({SUB: SUB_TEST}, "test: contract", ["Agent-Contract: run-1", "Agent-Contract-File: " + SUB,
                                                         "Agent-Contract-Id: tests.test_sub"])
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["failing"]), (tc.NOT_OK, ["tests.test_sub"]))
        self.commit(SUB_IMPL)
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"], res["files"]), (tc.OK, "green", [SUB]))

    def test_no_contract_commit_holds_nothing(self):
        self.commit(SUB_IMPL)
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"]), (tc.NO_CONTRACT, "no_contract"))
        self.assertNotIn(code, (tc.OK, tc.UNDECLARED))

    def test_a_rewritten_contract_test_does_not_hold(self):
        self.commit({SUB: SUB_TEST}, "test: contract", CONTRACT)
        self.commit({**SUB_IMPL, SUB: "def test_sub():\n    assert True\n"})
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"], res["modified"]), (tc.NOT_OK, "contract_modified", [SUB]))

    def test_a_changed_runner_file_does_not_hold(self):
        self.commit({SUB: SUB_TEST}, "test: contract", CONTRACT)
        self.commit({**SUB_IMPL, "tests/conftest.py": "import pytest\n"})
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"]), (tc.NOT_OK, "contract_modified"))

    def test_hold_takes_the_base_and_the_contract_from_the_harness(self):
        c = self.commit({SUB: SUB_TEST}, "test: contract", CONTRACT)
        self.commit(SUB_IMPL)
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None, base=self.base, contract=c)
        self.assertEqual((code, res["source"]), (tc.OK, "arguments"))
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["source"]), (tc.OK, "origin/HEAD and the trailer search"))
        # a commit that carries no trailers is no contract, whatever a trailer search would find
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None, base=self.base, contract=self.rev("HEAD"))
        self.assertEqual((code, res["result"]), (tc.NO_CONTRACT, "no_contract"))

    def test_a_contract_without_ids_or_files_is_never_green(self):
        self.commit({SUB: SUB_TEST}, "test: contract", ["Agent-Contract: run-1", "Agent-Contract-File: " + SUB])
        self.commit(SUB_IMPL)
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"]), (tc.NO_IDS, "contract_no_ids"))
        sh(self.repo, "git", "reset", "-q", "--hard", self.base)
        self.commit({SUB: SUB_TEST}, "test: contract", ["Agent-Contract: run-1", "Agent-Contract-Id: tests.test_sub"])
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"]), (tc.NO_FILES, "contract_no_files"))
        self.assertNotIn(code, (tc.OK, tc.UNDECLARED))

    def test_hold_refuses_a_base_that_is_not_an_ancestor_of_the_contract(self):
        c = self.commit({SUB: SUB_TEST}, "test: contract", CONTRACT)
        other = self.commit({"README": "x"})
        for base in (c, other):
            code, res = tc.hold(self.repo, dict(os.environ), lambda m: None, base=base, contract=c)
            self.assertEqual((code, res["result"]), (tc.NOT_ON_BASE, "contract_not_on_base"))

    def test_a_declaration_lost_after_a_contract_is_found_is_not_exit_2(self):
        self.commit({SUB: SUB_TEST}, "test: contract", CONTRACT)
        with mock.patch.object(tc, "declaration", side_effect=tc.Undeclared("gone")):
            code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"]), (tc.DECLARATION_LOST, "contract_declaration_lost"))
        self.assertNotIn(code, (tc.OK, tc.UNDECLARED))

    def test_hold_watches_every_test_file_the_contract_commit_touched(self):
        extra = "tests/test_extra.py"
        self.commit({SUB: SUB_TEST, extra: "def test_extra():\n    assert True\n"}, "test: contract", CONTRACT)
        self.commit({**SUB_IMPL, extra: "def test_extra():\n    assert False\n"})
        code, res = tc.hold(self.repo, dict(os.environ), lambda m: None)
        self.assertEqual((code, res["result"], res["modified"]), (tc.NOT_OK, "contract_modified", [extra]))

    def test_red_takes_the_base_from_the_harness(self):
        self.commit({SUB: SUB_TEST})
        code, res = tc.red(self.repo, self.rev("HEAD"), dict(os.environ), lambda m: None, base=self.base)
        self.assertEqual((code, res["base_source"]), (tc.OK, "arguments"))
        self.assertEqual(self.red()[1]["base_source"], "origin/HEAD")


class ShellTest(Fixture, unittest.TestCase):
    decl = SHELL_DECL
    files = {"t/run.sh": RUN_SH, "bin/greet": "#!/usr/bin/env bash\necho hello\n",
             "t/test-greet.sh": 'out="$(bash bin/greet)"; [ "$out" = hello ]\n'}

    def test_a_suite_is_one_id_its_file(self):
        self.commit({"t/test-name.sh": 'out="$(bash bin/greet ada)"; [ "$out" = "hello ada" ]\n'})
        code, res = self.red()
        self.assertEqual((code, res["ids"]), (tc.OK, ["t/test-name.sh"]), res)
        self.commit({"bin/greet": '#!/usr/bin/env bash\necho "hello${1:+ $1}"\n'})
        code, res = self.ci()
        self.assertEqual((code, res["result"]), (tc.OK, "green"), res)

    def test_a_suite_that_exits_77_is_a_skip_and_never_a_pass(self):
        self.commit({"t/test-later.sh": "exit 77\n"})
        self.assertEqual(self.red()[1]["result"], "contract_not_red")
        self.assertTrue(any("does not pass at the head (skip)" in w for w in self.ci()[1]["why"]))

    def test_a_pr_that_only_deletes_tests_is_green_reporting_the_deletions(self):
        self.commit({"bin/other": "#!/usr/bin/env bash\necho x\n"})
        sh(self.repo, "git", "rm", "-q", "t/test-greet.sh")
        sh(self.repo, "git", "commit", "-qm", "drop")
        code, res = self.ci()
        self.assertEqual((code, res["result"]), (tc.OK, "green"), res)
        self.assertIn("t/test-greet.sh", " ".join(res["why"]))
        self.assertNotEqual(res["why"], ["no changed test file"])

    def test_a_deletion_beside_a_test_is_noted_in_the_verdict(self):
        sh(self.repo, "git", "rm", "-q", "t/test-greet.sh")
        self.commit({"t/test-name.sh": 'out="$(bash bin/greet ada)"; [ "$out" = "hello ada" ]\n'})
        self.commit({"bin/greet": '#!/usr/bin/env bash\necho "hello${1:+ $1}"\n'})
        code, res = self.ci()
        self.assertTrue(any("deleted" in w and "t/test-greet.sh" in w for w in res["why"]), res)

    def test_a_bad_ref_is_refused_not_resolved(self):
        with self.assertRaises(tc.BadRef):
            tc.ci(self.repo, "--output=/tmp/x", "HEAD", dict(os.environ), lambda m: None)
        self.assertEqual(tc.main(["--repo", self.repo, "ci", "--base", "nope", "--head", "HEAD"]), tc.COULD_NOT_RUN)

    def test_a_descendant_that_escaped_the_group_cannot_hang_the_run(self):
        pidfile = os.path.join(self.tmp, "straggler.pid")
        decl = {"run": 'setsid sleep 30 & echo $! > "$PIDFILE"; echo hi', "minutes": 1}

        def reap():  # exactly the straggler this test started
            try:
                with open(pidfile) as f:
                    os.kill(int(f.read()), signal.SIGKILL)
            except (OSError, ValueError):
                pass
        self.addCleanup(reap)
        start = time.monotonic()
        r = tc.run(self.repo, decl, [], {**os.environ, "PIDFILE": pidfile}, "t", lambda m: None)
        self.assertLess(time.monotonic() - start, 15)
        self.assertEqual(r["tail"], ["hi"])

    def test_a_timeout_is_could_not_run_and_bounded(self):
        decl = {"run": "sleep 60 & sleep 60", "minutes": 0.03}
        start = time.monotonic()
        r = tc.run(self.repo, decl, [], dict(os.environ), "t", lambda m: None)
        self.assertLess(time.monotonic() - start, 20)
        self.assertEqual((r["ran"], r["timeout"]), (False, True))

    def test_the_buffered_output_is_capped(self):
        decl = {"run": "head -c 6000000 /dev/zero | tr '\\0' 'x'", "minutes": 1}
        r = tc.run(self.repo, decl, [], dict(os.environ), "t", lambda m: None)
        self.assertLessEqual(sum(len(x) for x in r["tail"]), tc.MAX_OUTPUT)

    def test_a_huge_or_crowded_report_is_no_report(self):
        d = tempfile.mkdtemp()
        big = os.path.join(d, "big.xml")
        with open(big, "w") as f:
            f.write("<testsuite>" + "<!--" + "x" * tc.MAX_REPORT_BYTES + "--><testcase name='a'/></testsuite>")
        self.assertIsNone(tc.junit(big))
        many = os.path.join(d, "many.xml")
        with open(many, "w") as f:
            f.write("<testsuite>" + "".join("<testcase name='%d'/>" % i for i in range(tc.MAX_CASES + 1)) + "</testsuite>")
        self.assertIsNone(tc.junit(many))

    def test_the_summary_escapes_ids_and_reasons(self):
        out = tc.summary({"result": "red", "why": ["<img src=x> | `bad`\nline"],
                          "rows": [{"id": "a|b`c<d>", "new": True, "base": "-", "overlay": "fail", "head": "pass"}]})
        self.assertNotIn("<img", out)
        self.assertNotIn("<d>", out)
        self.assertEqual(len(out.strip().splitlines()), 7, out)

    def test_an_undeclared_repository_is_exit_2(self):
        self.commit({".github/agent-checks.yaml": "version: 1\n"})
        sh(self.repo, "git", "push", "-q", "origin", "HEAD:main")
        sh(self.repo, "git", "fetch", "-q", "origin")
        self.assertEqual(tc.main(["--repo", self.repo, "red"]), tc.UNDECLARED)


class DeclarationTest(unittest.TestCase):
    TESTS = {"paths": ["t/"], "select": ["t/test-*.sh"], "runner": ["t/run.sh"], "run": 't/run.sh "$@"'}

    def test_the_tests_section_is_checked_strictly(self):
        doc = {"version": 1, "tests": dict(self.TESTS)}
        self.assertEqual(tc.validate(doc)["minutes"], tc.DEFAULT_MINUTES)
        for bad, why in (({"paths": []}, "paths"), ({"select": "t/*"}, "select"), ({"runner": ["/abs"]}, "runner"),
                         ({"run": " "}, "no run"), ({"timeoutMinutes": 16}, "timeoutMinutes"), ({"timeoutMinutes": True}, "timeoutMinutes"), ({"selcet": []}, "unknown keys")):
            with self.assertRaisesRegex(tc.Undeclared, why):
                tc.validate({"version": 1, "tests": {**self.TESTS, **bad}})

    @unittest.skipUnless(importlib.util.find_spec("agent_checks"), "agent_checks.py (v3 feature 3) is not beside this one")
    def test_agent_checks_accepts_the_section_and_keeps_its_steps(self):
        import agent_checks
        doc = {"version": 1, "steps": [{"name": "suites", "run": "true", "ci": "test"}], "tests": dict(self.TESTS)}
        self.assertEqual([s["name"] for s in agent_checks.validate(doc)["steps"]], ["suites"])


ACTION = os.path.join(HERE, "..", "..", ".github", "actions", "test-contract", "action.yml")


@unittest.skipUnless(os.path.exists(ACTION), "the composite action is not in this tree (the image's test stage)")
class ActionTest(unittest.TestCase):
    def test_the_action_runs_this_script_with_its_inputs_through_the_environment(self):
        with open(ACTION) as f:
            step = yaml.safe_load(f)["runs"]["steps"][0]
        script = step["env"]["SCRIPT"].replace("${{ github.action_path }}", os.path.dirname(ACTION))
        self.assertTrue(os.path.samefile(script, os.path.join(HERE, "test_contract.py")))
        self.assertNotIn("${{", step["run"], "inputs reach the shell through env, never inline")


if __name__ == "__main__":
    unittest.main()
