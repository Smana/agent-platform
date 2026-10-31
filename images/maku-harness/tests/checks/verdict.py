"""harness-checks' verdict on one agent-checks result: which steps that did not pass are the
harness's fault, and which are the checked repository's own verdict on its main.

A step that ran and ended fail is the repository's: its main is red, and the harness ran the check
as declared. So are the steps its stopOnFailure skipped after it. Everything else that is not a pass
is the harness's: a timeout, a budget spent, a stop with no failure before it, an unknown result.
Only a reasonless skip is declared.
"""

STOPPED = "an earlier step stopped the run"


def classify(steps):
    """Returns (failed, bad): the repository's failing steps, and the harness's faulty ones."""
    failed = [s["name"] for s in steps if s["result"] == "fail"]
    bad = []
    for s in steps:
        if s["result"] in ("pass", "fail"):
            continue
        if s["result"] == "skip" and (not s.get("reason") or (failed and s.get("reason") == STOPPED)):
            continue
        bad.append(s["name"])
    return failed, bad
