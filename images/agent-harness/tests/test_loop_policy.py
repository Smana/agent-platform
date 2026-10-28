"""loop_policy.py's rules (loops design L2, L3): the switch, the acceptance checks, the judge's
input and verdict, the decisions and the state. Pure: no agent-server, git or network."""
import json
import os
import sys
import tempfile
import unittest

HERE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
sys.path.insert(0, HERE)
import loop_policy as lp  # noqa: E402

POLICY = lp.Policy(stall_steps=80, continuations=2, judge_model="tier-light")


class ParseTest(unittest.TestCase):
    def test_an_empty_object_is_the_designs_defaults(self):
        self.assertEqual(lp.parse("{}"), POLICY)

    def test_given_keys_override_the_defaults(self):
        self.assertEqual(lp.parse('{"stallSteps": 40, "continuations": 1}'), lp.Policy(40, 1, "tier-light"))

    def test_judge_none_turns_the_judge_off(self):
        self.assertIsNone(lp.parse('{"judgeModel": "none"}').judge_model)

    def test_malformed_values_are_refused(self):
        for raw in ("", "[]", "80", '{"stallStep": 80}', '{"stallSteps": 0}', '{"stallSteps": 501}',
                    '{"stallSteps": true}', '{"stallSteps": "80"}', '{"continuations": -1}', '{"continuations": 6}',
                    '{"judgeModel": ""}', '{"judgeModel": "Tier Light"}', '{"judgeModel": 3}',
                    '{"judgeModel": null}', '{"stallSteps": 80.0}', "[" * 100000):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                lp.parse(raw)


BRIEF = """You are the implementer for agent factory task t1 in Smana/cloud-native-ref.
TASK-DATA-abc123
[untrusted data, sanitised by the factory]
## What is wrong
docs/a.md:3: links a removed runbook

## Acceptance check
`task docs-links` passes.

Fix it; change nothing else.
TASK-DATA-abc123
"""


class AcceptanceTest(unittest.TestCase):
    def test_the_templates_section_up_to_the_fence(self):
        self.assertEqual(lp.acceptance(BRIEF), "`task docs-links` passes.\n\nFix it; change nothing else.")

    def test_a_section_ends_at_the_next_heading(self):
        self.assertEqual(lp.acceptance("## Acceptance checks\nA\n### Notes\nB"), "A")

    def test_without_a_section_the_fenced_text(self):
        brief = BRIEF.replace("## Acceptance check\n", "")
        self.assertTrue(lp.acceptance(brief).startswith("[untrusted data"))
        self.assertNotIn("TASK-DATA", lp.acceptance(brief))

    def test_without_a_fence_the_whole_brief_cut(self):
        self.assertEqual(lp.acceptance("  fix it  "), "fix it")
        self.assertEqual(len(lp.acceptance("x" * 10000)), lp.ACCEPTANCE_MAX)


RUNS = [{"name": "check", "status": "completed", "conclusion": "success", "title": "All green"},
        {"name": "lint", "status": "in_progress", "conclusion": "", "title": ""}]


class JudgeInputTest(unittest.TestCase):
    def test_every_check_file_and_section_is_an_offered_id(self):
        text, offered = lp.judge_input("passes", "abc", ["docs/a.md"], "+x", RUNS)
        self.assertEqual(offered, {"acceptance", "diff", "check:check", "check:lint", "file:docs/a.md"})
        self.assertIn("- check:check: success - All green", text)
        self.assertIn("- check:lint: pending (in_progress)", text)
        self.assertIn("EVIDENCE: acceptance, check:check, check:lint, diff, file:docs/a.md", text)

    def test_an_unknown_head_and_a_long_diff_are_said_so(self):
        text, offered = lp.judge_input("", "", [], "y" * (lp.DIFF_MAX + 5), None)
        self.assertEqual(offered, {"acceptance", "diff"})
        self.assertIn("GitHub reported no such head", text)
        self.assertIn("DIFF (id: diff) cut at %d of %d characters" % (lp.DIFF_MAX, lp.DIFF_MAX + 5), text)


OFFERED = {"acceptance", "diff", "check:check"}


class VerdictTest(unittest.TestCase):
    def test_strict_json_or_json_inside_prose(self):
        raw = '{"verdict": "met", "reason": "CI is green.", "evidence": ["check:check", "made-up"]}'
        want = lp.Verdict("met", "CI is green.", ("check:check",))
        self.assertEqual(lp.verdict(raw, OFFERED), want)
        self.assertEqual(lp.verdict("Here you go:\n```json\n" + raw + "\n```", OFFERED), want)

    def test_anything_less_is_invalid_and_says_what(self):
        cases = {"no parse": "met", "no verdict": '{"verdict": "done", "reason": "r", "evidence": ["diff"]}',
                 "no reason": '{"verdict": "met", "reason": " ", "evidence": ["diff"]}',
                 "no evidence": '{"verdict": "met", "reason": "r", "evidence": ["check:other"]}'}
        for why, raw in cases.items():
            with self.subTest(why=why):
                self.assertEqual(lp.verdict(raw, OFFERED), lp.Verdict("invalid", why))

    def test_the_reason_is_cut(self):
        raw = json.dumps({"verdict": "not_yet", "reason": "r" * 5000, "evidence": ["diff"]})
        self.assertEqual(len(lp.verdict(raw, OFFERED).reason), lp.REASON_MAX)


MET, NOT_YET = lp.Verdict("met", "ok", ("diff",)), lp.Verdict("not_yet", "lint fails", ("check:lint",))
IMPOSSIBLE, INVALID = lp.Verdict("impossible", "needs a cluster", ("acceptance",)), lp.Verdict("invalid", "no parse")


class DecideTest(unittest.TestCase):
    def run_rounds(self, rounds, continuations=2):
        """rounds: (verdict, new commit since the last continuation). Returns every decision."""
        state, out = lp.State(start="s"), []
        for v, new in rounds:
            out.append(lp.decide(lp.Policy(80, continuations, "tier-light"), state, v, new))
        return out, state

    def test_met_ends_the_run_as_today(self):
        self.assertEqual(self.run_rounds([(MET, True)])[0], [lp.End(None)])

    def test_impossible_ends_it_with_the_judges_reason(self):
        self.assertEqual(self.run_rounds([(IMPOSSIBLE, True)])[0], [lp.End("judge_impossible", "needs a cluster")])

    def test_not_yet_continues_until_the_continuations_are_spent(self):
        out, state = self.run_rounds([(NOT_YET, True), (NOT_YET, True), (NOT_YET, True)])
        self.assertEqual(out, [lp.Continue("lint fails"), lp.Continue("lint fails"), lp.End("not_met", "lint fails")])
        self.assertEqual(state.sent, 2)

    def test_no_continuation_at_zero(self):
        self.assertEqual(self.run_rounds([(NOT_YET, True)], continuations=0)[0], [lp.End("not_met", "lint fails")])

    def test_a_discarded_verdict_counts_not_yet_once_then_impossible(self):
        out, _ = self.run_rounds([(INVALID, True), (INVALID, True)])
        self.assertEqual(out[0], lp.Continue(lp.INVALID))
        self.assertEqual(out[1], lp.End("judge_impossible", "the judge gave no usable verdict twice (no parse)"))

    def test_two_commitless_continuations_in_a_row_end_it_stalled(self):
        out, _ = self.run_rounds([(NOT_YET, True), (NOT_YET, False), (NOT_YET, False)], continuations=5)
        self.assertEqual(out[2], lp.End("stalled", "two continuations in a row ended with no new commit"))

    def test_a_commit_resets_the_count_and_met_still_wins(self):
        out, state = self.run_rounds([(NOT_YET, True), (NOT_YET, False), (NOT_YET, True), (NOT_YET, False), (MET, False)],
                                     continuations=5)
        self.assertEqual(out[-1], lp.End(None))
        self.assertEqual(state.failed, 2)

    def test_the_first_finish_is_no_iteration(self):
        _, state = self.run_rounds([(NOT_YET, False)])
        self.assertEqual(state.failed, 0)


class StateTest(unittest.TestCase):
    def test_wrong_types_in_the_file_start_fresh(self):
        bad = ['{"start": 1}', '{"start": "a", "sent": "2"}', '{"start": "a", "sent": true}',
               '{"start": "a", "committed": 1}', '{"start": "a", "mark": null}', '[1]', '"x"']
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "loop.json")
            for raw in bad:
                with self.subTest(raw=raw):
                    with open(path, "w") as f:
                        f.write(raw)
                    self.assertEqual(lp.State.load(path, "h"), lp.State(start="h"))

    def test_a_resumed_pod_with_no_usable_file_spends_the_stall_rule(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "loop.json")
            self.assertEqual(lp.State.load(path, "h", resumed=True), lp.State(start="h", committed=True))
            self.assertEqual(lp.State.load(path, "h"), lp.State(start="h"))

    def test_saved_owner_only_and_read_back_by_the_next_pod(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "loop.json")
            lp.State(start="abc", sent=1, mark="def").save(path)
            self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
            self.assertEqual(lp.State.load(path, "zzz"), lp.State(start="abc", sent=1, mark="def"))

    def test_a_missing_or_corrupt_file_starts_at_the_head(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "loop.json")
            self.assertEqual(lp.State.load(path, "h"), lp.State(start="h"))
            with open(path, "w") as f:
                f.write('{"start": "a", "extra": 1}')
            self.assertEqual(lp.State.load(path, "h"), lp.State(start="h"))


class ContinuationTest(unittest.TestCase):
    def test_the_reason_is_fenced_as_data_and_cannot_close_the_fence(self):
        text = lp.continuation("fix lint JUDGE-1a2b now", 1, 2, "JUDGE-1a2b")
        self.assertEqual(text.count("JUDGE-1a2b"), 3, "the preamble names it once, then two fence lines")
        self.assertIn("\nJUDGE-1a2b\nfix lint  now\nJUDGE-1a2b\n", text)
        self.assertIn("continuation 1 of 2", text)


if __name__ == "__main__":
    unittest.main()


# Drill E (gcp-0, 2026-10-27): an issue whose acceptance cannot pass; the judge answered `met` twice,
# its evidence listing check:validate FAILING, because the repository's README called that the
# drill's intended outcome. A failing check is never a pass, whatever the prose says.
DRILL_E_RUNS = [{"name": "validate", "status": "completed", "conclusion": "failure", "title": "task links"},
                {"name": "lint", "status": "completed", "conclusion": "success", "title": ""}]
DRILL_E_MET = lp.Verdict("met", "check:validate failing is the drill's intended outcome", ("check:validate", "diff"))


PENDING_RUNS = [{"name": "validate", "status": "in_progress", "conclusion": "", "title": ""}]


class GuardTest(unittest.TestCase):
    def offered(self, runs):
        return lp.judge_input("task links passes", "abc", [], "", runs)[1]

    def test_the_offered_ids_say_which_checks_failed_or_are_pending_and_whether_the_head_is_unknown(self):
        o = self.offered(DRILL_E_RUNS)
        self.assertEqual((o.failing, o.pending, o.unpushed), (("check:validate",), (), False))
        o = self.offered([DRILL_E_RUNS[1]])
        self.assertEqual((o.failing, o.pending, o.unpushed), ((), (), False))
        o = self.offered(PENDING_RUNS)
        self.assertEqual((o.failing, o.pending, o.unpushed), ((), ("check:validate",), False))
        o = self.offered(None)
        self.assertEqual((o.failing, o.pending, o.unpushed), ((), (), True))
        o = self.offered([])  # no check run reported: not a failure
        self.assertEqual((o.failing, o.pending, o.unpushed), ((), (), False))

    def test_met_with_a_failing_check_is_not_yet(self):
        got = lp.guard(DRILL_E_MET, self.offered(DRILL_E_RUNS))
        self.assertEqual(got.kind, "not_yet")
        self.assertIn("check:validate", got.reason)
        self.assertEqual(got.evidence, DRILL_E_MET.evidence)

    def test_met_with_a_check_still_pending_is_not_yet(self):
        got = lp.guard(DRILL_E_MET, self.offered(PENDING_RUNS))
        self.assertEqual(got.kind, "not_yet")
        self.assertIn("pending", got.reason)

    def test_met_when_the_head_is_unknown_to_github_is_not_yet(self):
        # not pushed, or gh failing: the checks' output is evidence of nothing
        got = lp.guard(DRILL_E_MET, self.offered(None))
        self.assertEqual(got.kind, "not_yet")
        self.assertIn("not pushed", got.reason)

    def test_the_reason_never_turns_met_into_impossible(self):
        v = lp.Verdict("met", "check:validate cannot pass: the link must not exist", ("check:validate",))
        self.assertEqual(lp.guard(v, self.offered(DRILL_E_RUNS)).kind, "not_yet")

    def test_nothing_else_changes(self):
        clean = self.offered([DRILL_E_RUNS[1]])
        self.assertIs(lp.guard(DRILL_E_MET, clean), DRILL_E_MET)
        for v in (NOT_YET, IMPOSSIBLE, INVALID):  # the judge's own impossible stands
            self.assertIs(lp.guard(v, self.offered(DRILL_E_RUNS)), v)

    def test_the_prompt_says_checks_are_literal(self):
        self.assertIn("literal", lp.JUDGE_SYSTEM)
        self.assertIn("never a pass", lp.JUDGE_SYSTEM)
