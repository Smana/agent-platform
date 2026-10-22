"""agent-run's half of v3 feature 3 (offline proof 5): the rule, the milestone, the prefetch, the final
run on the head, its note and its deadline. agent-checks itself is test_agent_checks.py's."""
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import agent_run  # noqa: E402

DECL = {"minutes": 8, "tools": [], "definedBy": [], "steps": [], "ciOnly": []}
# A stand-in agent-checks: writes the result its first argument names for the head, counts its runs.
FAKE_TOOL = r'''#!{python}
import json, os, subprocess, sys
state = os.environ["FAKE_CHECKS_DIR"]
head = subprocess.run(["git", "-C", os.environ["FAKE_REPO"], "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
with open(os.path.join(state, "calls"), "a") as f:
    f.write(" ".join(sys.argv[1:]) + "\n")
steps = [{"name": "suites", "result": "pass"}, {"name": "pre-commit", "result": "fail"}]
json.dump({"version": 1, "head": head, "dirty": False, "partial": False, "steps": steps}, open(os.path.join(state, head + ".json"), "w"))
'''


class FakeNotes:
    def __init__(self, limited=0):
        self.sent, self.limited, self.last = [], limited, None

    @staticmethod
    def clock():
        return 0.0

    def send(self, text):
        if self.limited:
            self.limited -= 1
            return False
        self.sent.append(text)
        return True


class RuleTest(unittest.TestCase):
    def test_agent_checks_reaches_the_checks_milestone(self):
        self.assertEqual(agent_run.milestones("agent-checks"), {"checks"})
        self.assertEqual(agent_run.milestones("agent-checks --step suites"), {"checks"})
        self.assertEqual(agent_run.milestones("echo agent-checks"), set())

    def test_the_rule_is_the_roles_and_only_on_a_check_enabled_run_that_declares(self):
        implementer = agent_run.checks_rules({"ROLE": "implementer"}, DECL)
        self.assertIn("Before every push, run `agent-checks` and fix what it reports. It runs the CI's fast subset; "
                      "the CI stays the judge. If a failure is not your change's, say so in the pull request.", implementer)
        self.assertIn("cite its first line as your evidence", agent_run.checks_rules({"ROLE": "reviewer"}, DECL))
        self.assertEqual(agent_run.checks_rules({"ROLE": "tester"}, DECL), "")
        self.assertEqual(agent_run.checks_rules({"ROLE": "implementer"}, None), "")

    def test_nothing_is_declared_without_the_switch(self):
        with mock.patch.object(agent_run, "checks_module") as module:
            self.assertIsNone(agent_run.declared({}))
        module.assert_not_called()


class FinalChecksTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.repo, self.state = os.path.join(self.tmp, "repo"), os.path.join(self.tmp, "state")
        os.makedirs(self.state)
        subprocess.run(["git", "init", "-q", self.repo], check=True)
        subprocess.run(["git", "-C", self.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q",
                        "--allow-empty", "-m", "head"], check=True)
        self.head = subprocess.run(["git", "-C", self.repo, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
        tool = os.path.join(self.tmp, "agent-checks")
        with open(tool, "w") as f:
            f.write(FAKE_TOOL.replace("{python}", sys.executable, 1))
        os.chmod(tool, 0o755)
        for name, value in (("REPO_DIR", self.repo), ("CHECKS_DIR", self.state), ("CHECKS_TOOL", tool)):
            p = mock.patch.object(agent_run, name, value)
            p.start()
            self.addCleanup(p.stop)
        p = mock.patch.dict(os.environ, {"FAKE_CHECKS_DIR": self.state, "FAKE_REPO": self.repo})
        p.start()
        self.addCleanup(p.stop)
        self.env = {"ROLE": "implementer", "AGENT_CHECKS": "1"}

    def calls(self):
        try:
            with open(os.path.join(self.state, "calls")) as f:
                return f.read().splitlines()
        except FileNotFoundError:
            return []

    def test_off_or_a_reviewer_runs_nothing(self):
        self.assertIsNone(agent_run.final_checks(self.env, None))
        self.assertIsNone(agent_run.final_checks({**self.env, "ROLE": "reviewer"}, DECL))
        self.assertEqual(self.calls(), [])

    def test_the_head_is_checked_and_the_note_names_the_failed_step(self):
        notes = FakeNotes()
        res = agent_run.final_checks(self.env, DECL, notes)
        self.assertEqual(self.calls(), [""])
        self.assertEqual(res["head"], self.head)
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s: 1 pass, 1 fail. Failed: pre-commit." % self.head[:12]])

    def test_the_agents_full_clean_result_for_the_head_stands(self):
        with open(os.path.join(self.state, self.head + ".json"), "w") as f:
            json.dump({"head": self.head, "dirty": False, "partial": False, "steps": [{"name": "suites", "result": "pass"}]}, f)
        notes = FakeNotes()
        agent_run.final_checks(self.env, DECL, notes)
        self.assertEqual(self.calls(), [])
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s: 1 pass (the agent's own run)." % self.head[:12]])

    def test_a_partial_or_dirty_result_is_run_again(self):
        for stale in ({"partial": True}, {"dirty": True}):
            with self.subTest(stale=stale):
                with open(os.path.join(self.state, self.head + ".json"), "w") as f:
                    json.dump({"head": self.head, "steps": [], **stale}, f)
                before = len(self.calls())
                agent_run.final_checks(self.env, DECL, FakeNotes())
                self.assertEqual(len(self.calls()), before + 1)

    def test_too_little_time_left_records_a_skip(self):
        notes = FakeNotes()
        res = agent_run.final_checks({**self.env, "AGENT_RUN_SECONDS": "600"}, DECL, notes, started=0.0, clock=lambda: 100.0)
        self.assertEqual(res, {"version": 1, "head": self.head, "skipped": "deadline"})
        self.assertEqual(self.calls(), [])
        with open(os.path.join(self.state, self.head + ".json")) as f:
            self.assertEqual(json.load(f)["skipped"], "deadline")
        self.assertEqual(notes.sent, ["Harness (implementer): checks on %s skipped: deadline." % self.head[:12]])

    def test_enough_time_left_runs(self):
        agent_run.final_checks({**self.env, "AGENT_RUN_SECONDS": "7200"}, DECL, FakeNotes(), started=0.0, clock=lambda: 100.0)
        self.assertEqual(self.calls(), [""])

    def test_a_rate_limited_note_waits_out_the_minute_once(self):
        notes, slept = FakeNotes(limited=1), []
        agent_run.post_once(notes, "the note", sleep=slept.append)
        self.assertEqual((slept, notes.sent), ([agent_run.MIN_NOTE_GAP_S], ["the note"]))

    def test_the_final_run_is_a_span_that_says_why_it_failed(self):
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(exporter))
        agent_run.final_checks(self.env, DECL, None, lambda name: provider.get_tracer("t").start_span(name))
        (span,) = exporter.get_finished_spans()
        self.assertEqual((span.name, dict(span.attributes), span.status.status_code.name),
                         ("agent-run.checks", {"error.type": "checks_failed"}, "ERROR"))

    def test_the_note_fits_the_brokers_280_characters(self):
        steps = [{"name": "step-%02d-%s" % (i, "x" * 30), "result": "fail"} for i in range(20)]
        text = agent_run.checks_note("implementer", {"head": self.head, "steps": steps}, False)
        self.assertLessEqual(len(text), agent_run.NOTE_MAX)
        self.assertTrue(text.endswith(", ….") and "Failed: step-00" in text)


class MainChecksTest(unittest.TestCase):
    def _main(self, env, poll=0, decl=DECL):
        with tempfile.TemporaryDirectory() as d:
            task = os.path.join(d, "task.md")
            open(task, "w").write("rules")
            env = {"TASK_FILE": task, "RULES_FILE": task, "ROLE": "implementer", **env}
            with mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch("agent_run.subprocess.Popen"), \
                    mock.patch("agent_run.wait_ready"), \
                    mock.patch("agent_run.persisted", return_value=False), \
                    mock.patch("agent_run.clone", return_value=False), \
                    mock.patch("agent_run.checks_module") as module, \
                    mock.patch("agent_run.prefetch") as pre, \
                    mock.patch("agent_run.final_checks") as final, \
                    mock.patch("agent_run.build_request", return_value={"x": 1}) as build, \
                    mock.patch("agent_run.http", return_value={"id": "c1"}), \
                    mock.patch("agent_run.poll", return_value=poll), \
                    mock.patch("agent_run.StepLog"), \
                    mock.patch("agent_run.stop", return_value="stopped"), \
                    mock.patch("agent_run.timed"):
                module.return_value.declaration.return_value = ("base", "blob", decl)
                agent_run.main()
        return pre, final, build.call_args.args[2]

    def test_a_check_enabled_run_prefetches_states_the_rule_and_checks_the_finished_head(self):
        pre, final, rules = self._main({"AGENT_CHECKS": "1"})
        pre.assert_called_once()
        self.assertTrue(rules.startswith("rules\nRepository checks:\n- Before every push"))
        self.assertEqual(final.call_args.args[1], DECL)

    def test_without_the_switch_nothing_changes(self):
        pre, final, rules = self._main({})
        pre.assert_not_called()
        self.assertEqual(rules, "rules")
        self.assertIsNone(final.call_args.args[1])

    def test_a_failed_conversation_is_not_checked(self):
        _, final, _ = self._main({"AGENT_CHECKS": "1"}, poll=1)
        final.assert_not_called()


if __name__ == "__main__":
    unittest.main()
