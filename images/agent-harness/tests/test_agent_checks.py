"""agent-checks (v3 feature 3, offline proof 3): the declaration from the default branch, the bounds,
the summary, the step's environment, the exit codes.

Each case builds a repository with an origin whose main declares the checks, clones it, and runs
agent_checks.main() in-process against a fake mise that installs a tool by making its directory.
"""
import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_checks  # noqa: E402

FAKE_MISE = r'''#!{python}
"""A stand-in for mise: `where`, `install`, `env --json`, on $MISE_DATA_DIR/installs/<tool>/<version>.
mise gets only the environment agent-checks gives it, so its knobs sit beside it in fake-mise.json."""
import json, os, sys, time
here = os.path.dirname(os.path.abspath(sys.argv[0]))
knobs = json.load(open(os.path.join(here, "fake-mise.json"))) if os.path.exists(os.path.join(here, "fake-mise.json")) else {}
data = os.path.join(os.environ["MISE_DATA_DIR"], "installs")
def where(spec):
    t, v = spec.split("@", 1)
    return os.path.join(data, t, v)
cmd, specs = sys.argv[1], [a for a in sys.argv[2:] if not a.startswith("-")]
if cmd == "where":
    sys.exit(0 if os.path.isdir(where(specs[0])) else 1)
if cmd == "install":
    with open(os.path.join(here, "mise.log"), "a") as log:
        log.write(json.dumps({"specs": specs, "token": os.environ.get("GITHUB_TOKEN", "")}) + "\n")
    time.sleep(knobs.get("sleep", 0))
    for s in specs:
        if s.split("@")[0] == knobs.get("fail"):
            continue
        b = os.path.join(where(s), "bin")
        os.makedirs(b, exist_ok=True)
        tool = os.path.join(b, s.split("@")[0])
        with open(tool, "w") as f:
            f.write("#!/bin/sh\necho %s\n" % s)
        os.chmod(tool, 0o755)
    sys.exit(0)
if cmd == "env":
    bins = [os.path.join(where(s), "bin") for s in specs]
    print(knobs["env_raw"] if "env_raw" in knobs else json.dumps({"PATH": ":".join(bins + [os.environ["PATH"]])}))
    sys.exit(0)
sys.exit(3)
'''

DECLARED = """version: 1
timeoutMinutes: 2
tools: [task, go]
definedBy: [taskfile.yaml, scripts/ci/, mise.toml]
steps:
  - {name: first, run: "task > first.out; go >> first.out", ci: validate, stopOnFailure: true}
  - {name: second, run: "echo second", ci: lint}
ciOnly:
  - {check: prerelease, part: all, why: "signs with the workflow's identity"}
"""


def sh(cwd, *args, **kw):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True, **kw).stdout.strip()


class Rig(unittest.TestCase):
    """A clone of an origin whose main declares DECLARED, on the branch agent/x."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        seed, self.origin, self.repo = (os.path.join(self.tmp, n) for n in ("seed", "origin.git", "repo"))
        git = ["git", "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"]
        os.makedirs(os.path.join(seed, ".github"))
        sh(self.tmp, *git, "init", "-q", seed)
        self.write(seed, ".github/agent-checks.yaml", DECLARED)
        self.write(seed, "mise.toml", '[tools]\ntask = "3.54.0"\ngo = {version = "1.27.2"}\n')
        self.write(seed, "taskfile.yaml", "version: '3'\n")
        sh(seed, *git, "add", "-A")
        sh(seed, *git, "commit", "-qm", "seed")
        sh(self.tmp, "git", "clone", "-q", "--bare", seed, self.origin)
        sh(self.tmp, "git", "clone", "-q", self.origin, self.repo)
        sh(self.repo, "git", "checkout", "-qb", "agent/x")
        self.git = git
        mise = os.path.join(self.tmp, "mise")
        self.write(self.tmp, "mise", FAKE_MISE.replace("{python}", sys.executable, 1))
        os.chmod(mise, 0o755)
        self.mise_log = os.path.join(self.tmp, "mise.log")
        # A credential helper that answers with a token, as git-credential-agent does in a run.
        self.write(self.tmp, "gitconfig", '[credential]\n\thelper = "!f() { echo username=x-access-token; echo password=ghs_installonly; }; f"\n')
        self.env = {"PATH": os.environ["PATH"], "HOME": self.tmp,
                    "GIT_CONFIG_GLOBAL": os.path.join(self.tmp, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1",
                    "CI": "true", "GITHUB_ACTIONS": "true", "GH_TOKEN": "ghs_x", "GIT_TOKEN_FILE": "/run/agent/github/token",
                    "LLM_API_KEY_FILE": "/run/agent/model/api_key", "OH_SECRET_KEY": "k", "MISE_DATA_DIR": "/elsewhere"}
        for name, value in {"REPO": self.repo, "STATE": os.path.join(self.tmp, "state"),
                            "TOOLCHAIN": os.path.join(self.tmp, "toolchain"), "MISE": mise,
                            "MINUTE": 1, "HEARTBEAT_S": 0.2, "KILL_GRACE_S": 1, "INSTALL_S": 5}.items():
            patcher = mock.patch.object(agent_checks, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    @staticmethod
    def write(root, rel, text):
        with open(os.path.join(root, rel), "w") as f:
            f.write(text)

    def commit(self, rel, text):
        self.write(self.repo, rel, text)
        sh(self.repo, *self.git, "commit", "-qam", "change " + rel)

    def base_declares(self, decl):
        """main declares decl: the default branch moves, and the agent's branch with it."""
        self.write(self.repo, ".github/agent-checks.yaml", decl)
        sh(self.repo, *self.git, "commit", "-qam", "declare")
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))

    def run_checks(self, *argv, env=None):
        """(exit code, stdout lines with their arrival times)."""
        lines = []

        class Clock(io.TextIOBase):
            def write(self, s):
                for line in s.splitlines():
                    lines.append((time.monotonic(), line))
                return len(s)
        with mock.patch.dict(os.environ, {**self.env, **(env or {})}, clear=True), contextlib.redirect_stdout(Clock()):
            code = agent_checks.main(list(argv))
        return code, lines

    def result(self, stem=None):
        head = stem or sh(self.repo, "git", "rev-parse", "HEAD")
        with open(os.path.join(agent_checks.STATE, head + ".json")) as f:
            return json.load(f)

    def text(self, lines):
        return "\n".join(line for _, line in lines)


class DeclarationTest(Rig):
    def test_the_default_branch_declares_and_a_branch_edit_changes_nothing(self):
        self.commit(".github/agent-checks.yaml", DECLARED.replace('echo second', 'exit 1'))
        code, _ = self.run_checks()
        res = self.result()
        self.assertEqual(code, 0)
        self.assertEqual([s["result"] for s in res["steps"]], ["pass", "pass"])
        self.assertEqual(res["declaration"], sh(self.repo, "git", "rev-parse", "origin/main:.github/agent-checks.yaml"))

    def test_nothing_declared_exits_2(self):
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))
        os.remove(os.path.join(self.repo, ".github/agent-checks.yaml"))
        sh(self.repo, *self.git, "commit", "-qam", "drop")
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))
        code, lines = self.run_checks()
        self.assertEqual(code, 2)
        self.assertIn("declares no checks", self.text(lines))

    def test_a_malformed_declaration_exits_2_with_its_reason(self):
        for bad, why in ((DECLARED.replace("version: 1", "version: 2"), "version must be 1"),
                         (DECLARED.replace("name: second", "name: First"), "step names"),
                         (DECLARED.replace("timeoutMinutes: 2", "timeoutMinutes: 16"), "timeoutMinutes must be 1 to 15")):
            with self.subTest(why=why):
                with self.assertRaisesRegex(agent_checks.Undeclared, why):
                    agent_checks.validate(__import__("yaml").safe_load(bad))

    def test_a_binary_declaration_exits_2(self):
        self.write(self.repo, ".github/agent-checks.yaml", "")
        with open(os.path.join(self.repo, ".github/agent-checks.yaml"), "wb") as f:
            f.write(b"version: 1\n\xff\xfe\x80")
        sh(self.repo, *self.git, "commit", "-qam", "binary")
        sh(self.repo, "git", "update-ref", "refs/remotes/origin/main", sh(self.repo, "git", "rev-parse", "HEAD"))
        code, _ = self.run_checks()
        self.assertEqual(code, 2)

    def test_a_boolean_is_not_a_timeout(self):
        with self.assertRaisesRegex(agent_checks.Undeclared, "timeoutMinutes"):
            agent_checks.validate(__import__("yaml").safe_load(DECLARED.replace("timeoutMinutes: 2", "timeoutMinutes: true")))

    def test_mise_env_printing_something_unusable_exits_1(self):
        for raw, why in (("not json", "no JSON object"), ("[1]", "not a string"), ('{"PATH": 1}', "not a string"),
                         ('{"A": null}', "not a string")):
            with self.subTest(raw=raw):
                self.write(self.tmp, "fake-mise.json", json.dumps({"env_raw": raw}))
                code, lines = self.run_checks()
                self.assertEqual(code, 1)
                self.assertIn(why, self.text(lines))

    def test_a_step_boolean_timeout_is_refused(self):
        with self.assertRaisesRegex(agent_checks.Undeclared, "step first: timeoutMinutes"):
            agent_checks.validate(__import__("yaml").safe_load(DECLARED.replace("stopOnFailure: true", "stopOnFailure: true, timeoutMinutes: true")))

    def test_an_unknown_step_exits_2(self):
        code, lines = self.run_checks("--step", "third")
        self.assertEqual(code, 2)
        self.assertIn("declared: first, second", self.text(lines))


class ToolchainTest(Rig):
    def test_the_branchs_pins_are_installed_with_the_token_and_found_first_on_path(self):
        self.commit("mise.toml", '[tools]\ntask = "3.55.0"\ngo = {version = "1.27.2"}\n')
        code, _ = self.run_checks()
        self.assertEqual(code, 0)
        with open(self.mise_log) as f:
            installs = [json.loads(line) for line in f]
        self.assertEqual(installs, [{"specs": ["task@3.55.0", "go@1.27.2"], "token": "ghs_installonly"}])
        with open(os.path.join(self.repo, "first.out")) as f:
            self.assertEqual(f.read(), "task@3.55.0\ngo@1.27.2\n")

    def test_a_second_run_installs_nothing(self):
        self.run_checks()
        self.run_checks()
        with open(self.mise_log) as f:
            self.assertEqual(len(f.readlines()), 1)

    def test_an_install_past_its_bound_is_stopped_and_the_steps_say_why(self):
        self.write(self.tmp, "fake-mise.json", '{"sleep": 30}')
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(code, 1)
        self.assertEqual(res["missingTools"], ["task", "go"])
        self.assertEqual(res["steps"][0]["reason"], "toolchain incomplete")
        self.assertIn("toolchain incomplete: task, go", self.text(lines))

    def test_a_tool_mise_toml_does_not_pin_is_missing(self):
        self.commit("mise.toml", '[tools]\ntask = "3.54.0"\n')
        self.run_checks()
        self.assertEqual(self.result()["missingTools"], ["go"])

    def test_install_only(self):
        code, lines = self.run_checks("--install")
        self.assertEqual(code, 0)
        self.assertIn("agent-checks: toolchain ready", self.text(lines))
        self.assertFalse(os.path.exists(os.path.join(self.repo, "first.out")))


class StepTest(Rig):
    def test_the_step_environment_holds_no_ci_no_key_and_no_token_path(self):
        self.base_declares(DECLARED.replace("echo second", "env > env.out"))
        self.run_checks()
        with open(os.path.join(self.repo, "env.out")) as f:
            names = {line.split("=", 1)[0] for line in f if "=" in line}
        for name in ("CI", "GITHUB_ACTIONS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_TOKEN_FILE", "LLM_API_KEY_FILE",
                     "OH_SECRET_KEY", "MISE_DATA_DIR"):
            self.assertNotIn(name, names)
        self.assertIn("XDG_CACHE_HOME", names)

    def test_a_step_past_its_timeout_is_killed_with_its_children(self):
        self.base_declares(DECLARED.replace('{name: second, run: "echo second", ci: lint}',
                                            '{name: second, run: "sleep 60 & echo $! > child.pid; wait", ci: lint, timeoutMinutes: 1}'))
        code, _ = self.run_checks()
        self.assertEqual(code, 1)
        self.assertEqual(self.result()["steps"][1]["result"], "timeout")
        with open(os.path.join(self.repo, "child.pid")) as f:
            pid = int(f.read())
        time.sleep(0.2)
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)

    def test_a_runaway_writer_is_bounded_in_the_log_and_the_tail(self):
        self.base_declares(DECLARED.replace('echo second', "head -c 3000000 /dev/zero | tr '\\\\0' x; echo; echo end; exit 1"))
        with mock.patch.object(agent_checks, "LOG_CAP", 1000):
            self.run_checks()
        res = self.result()
        with open(os.path.join(agent_checks.STATE, res["head"] + ".log")) as f:
            log = f.read()
        self.assertIn("output truncated at 1000 bytes", log)
        self.assertLess(len(log.encode()), 1000 + 200)  # the cap, then the one marker line
        with open(os.path.join(agent_checks.STATE, res["head"] + ".txt")) as f:
            self.assertLess(len(f.read()), agent_checks.SUMMARY_CAP + 1)
        tails = []
        with mock.patch.object(agent_checks, "summary", side_effect=lambda r: tails.extend(t for s in r["steps"] for t in s["tail"]) or "x"):
            self.run_checks()
        self.assertTrue(tails and max(map(len, tails)) <= agent_checks.LINE_CAP)

    def test_stop_on_failure_skips_the_rest(self):
        self.base_declares(DECLARED.replace("task > first.out; go >> first.out", "exit 3"))
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(code, 1)
        self.assertEqual([(s["result"], s.get("reason")) for s in res["steps"]],
                         [("fail", None), ("skip", "an earlier step stopped the run")])
        self.assertEqual(res["steps"][0]["exit"], 3)

    def test_a_line_at_least_every_heartbeat(self):
        self.base_declares(DECLARED.replace('echo second', 'sleep 1.5'))
        _, lines = self.run_checks()
        gaps = [b[0] - a[0] for a, b in zip(lines, lines[1:])]
        self.assertLess(max(gaps), agent_checks.HEARTBEAT_S + 0.5)
        self.assertIn("agent-checks: second running", self.text(lines))

    def test_one_step(self):
        code, _ = self.run_checks("--step", "second")
        head = sh(self.repo, "git", "rev-parse", "HEAD")
        self.assertEqual(code, 0)
        self.assertTrue(self.result(head + ".step-second")["partial"])
        self.assertFalse(os.path.exists(os.path.join(agent_checks.STATE, head + ".json")))



class SummaryTest(Rig):
    def test_the_summary_is_capped_and_keeps_its_frame(self):
        noisy = "for i in $(seq 1 200); do printf 'line %d %0400d\\n' $i 0; done; exit 1"
        decl = DECLARED.replace('echo second', noisy).replace("ci: lint}", "ci: lint}\n  - {name: third, run: \"%s\", ci: lint}" % noisy)
        self.base_declares(decl)
        code, lines = self.run_checks()
        head = sh(self.repo, "git", "rev-parse", "HEAD")
        with open(os.path.join(agent_checks.STATE, head + ".txt")) as f:
            text = f.read().rstrip("\n")
        self.assertEqual(code, 1)
        self.assertLessEqual(len(text), agent_checks.SUMMARY_CAP)
        self.assertIn("2 fail", text.splitlines()[0])
        self.assertIn("--- second, last lines ---", text)
        self.assertIn("line 200 ", text)
        self.assertIn("  prerelease: all (signs with the workflow's identity)", text)
        self.assertTrue(text.endswith("a green result here is not the CI's."))

    def test_definition_touched_lists_the_changed_definition_paths(self):
        self.commit("taskfile.yaml", "version: '3'\n# changed\n")
        self.write(self.repo, "mise.toml", '[tools]\ntask = "3.54.0"\ngo = "1.27.2"\n')
        code, lines = self.run_checks()
        res = self.result()
        self.assertEqual(res["definitionTouched"], ["mise.toml", "taskfile.yaml"])
        self.assertTrue(res["dirty"])
        self.assertIn("unverified until the CI runs", self.text(lines))

    def test_files_the_checks_change_are_named(self):
        self.base_declares(DECLARED.replace('echo second', 'echo fixed >> taskfile.yaml'))
        self.run_checks()
        self.assertEqual(self.result()["changedByChecks"], ["taskfile.yaml"])

    def test_each_run_is_one_ledger_line(self):
        self.run_checks()
        self.run_checks("--step", "second")
        with open(os.path.join(agent_checks.STATE, agent_checks.LEDGER)) as f:
            rows = [json.loads(line) for line in f]
        self.assertEqual([(r["partial"], r["counts"]) for r in rows], [(False, {"pass": 2}), (True, {"pass": 1})])


if __name__ == "__main__":
    unittest.main()
