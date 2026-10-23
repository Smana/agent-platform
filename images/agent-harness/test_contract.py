#!/agent-server/.venv/bin/python
"""test-contract: the test-first tester's contract check (v3 feature 4, TF5 to TF7).

One script for the sandbox and the CI, so they agree. The repository declares its tests in the
`tests` section of .github/agent-checks.yaml, always read from the base, never from the change:

  tests:
    paths:  [scripts/ci/tests/]                 # what a tester may write; a prefix ending in / or a path
    select: ["scripts/ci/tests/test-*.sh"]      # globs: the changed files whose tests are run
    runner: [scripts/ci/tests/run.sh]           # the runner's own files: never a contract, red when changed
    run: scripts/ci/tests/run.sh "$@"           # bash; "$@" the selected files; writes JUnit XML to $AGENT_TEST_REPORT
    timeoutMinutes: 5

  test-contract red [--commit SHA]   the tester's red check: base = the default branch (origin/HEAD)
  test-contract hold                 the implementer's contract, run at HEAD
  test-contract ci --base SHA --head SHA [--summary FILE]   the "Test contract" job

An id is a JUnit testcase's `classname::name`. A suite with no finer report writes one testcase, its
file. The runner ran when its report exists and holds a testcase: an infrastructure failure is never
red. The overlay is the base's tree with the changed test files at their new content.
"""
import argparse
import contextlib
import fnmatch
import json
import os
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import xml.etree.ElementTree as ET

import yaml

DECLARATION = ".github/agent-checks.yaml"
DEFAULT_BRANCH = "refs/remotes/origin/HEAD"
DEFAULT_MINUTES, MAX_MINUTES = 5, 15
HEARTBEAT_S = 15
MAX_IDS = 50
# Exit codes, the harness's ends (TF5) and the job's conclusion (TF7).
OK, NOT_OK, UNDECLARED, OUTSIDE, COULD_NOT_RUN = 0, 1, 2, 3, 4
# The runner's shell, resolved from the caller's PATH: a step's PATH is the toolchain's.
BASH = shutil.which("bash") or "/bin/bash"
TRAILER_RUN, TRAILER_FILE, TRAILER_ID = "Agent-Contract", "Agent-Contract-File", "Agent-Contract-Id"


class Undeclared(Exception):
    """The base declares no tests: exit 2."""


def git(repo: str, *args: str, check: bool = True) -> str:
    out = subprocess.run(["git", "-C", repo, *args], capture_output=True, text=True)
    if check and out.returncode:
        raise RuntimeError("git %s: %s" % (" ".join(args), out.stderr.strip()))
    return out.stdout


def validate(doc) -> dict:
    """The declaration's tests section, checked as each repository's coverage suite checks it."""
    def need(cond, why):
        if not cond:
            raise Undeclared("%s: tests: %s" % (DECLARATION, why))
    need(isinstance(doc, dict) and isinstance(doc.get("tests"), dict), "no tests section")
    t = doc["tests"]
    need(not set(t) - {"paths", "select", "runner", "run", "timeoutMinutes"}, "unknown keys %s" % sorted(set(t) - {"paths", "select", "runner", "run", "timeoutMinutes"}))
    for key in ("paths", "select", "runner"):
        v = t.get(key)
        need(isinstance(v, list) and v and all(isinstance(p, str) and p and not p.startswith("/") for p in v),
             "%s must be a non-empty list of relative paths" % key)
    need(isinstance(t.get("run"), str) and t["run"].strip(), "no run")
    m = t.get("timeoutMinutes", DEFAULT_MINUTES)
    need(isinstance(m, int) and 1 <= m <= MAX_MINUTES, "timeoutMinutes must be 1 to %d" % MAX_MINUTES)
    return {"paths": t["paths"], "select": t["select"], "runner": t["runner"], "run": t["run"], "minutes": m,
            "tools": doc.get("tools", [])}


def declaration(repo: str, ref: str) -> dict:
    """The tests section at ref: the base's, as policy-bot reads .policy.yml from main."""
    raw = git(repo, "show", "%s:%s" % (ref, DECLARATION), check=False)
    if not raw:
        raise Undeclared("%s declares no checks at %s" % (DECLARATION, ref[:12]))
    try:
        return validate(yaml.safe_load(raw))
    except yaml.YAMLError as exc:
        raise Undeclared("%s: %s" % (DECLARATION, exc)) from exc


def under(path: str, prefixes: list[str]) -> bool:
    return any(path == p or (p.endswith("/") and path.startswith(p)) for p in prefixes)


def changes(repo: str, base: str, head: str) -> list[tuple[str, str]]:
    """(status, path) the change makes since its merge base with base; a rename is a delete and an add."""
    out = git(repo, "diff", "--name-status", "--no-renames", "-z", "%s...%s" % (base, head))
    parts = out.split("\0")
    return [(parts[i][0], parts[i + 1]) for i in range(0, len(parts) - 1, 2)]


@contextlib.contextmanager
def tree(repo: str, sha: str, head: str | None = None, files=(), gone=()):
    """A clean, detached checkout of sha; with head, the overlay: files at head's content, gone removed."""
    where = tempfile.mkdtemp(prefix="test-contract-")
    git(repo, "worktree", "add", "--detach", "-q", where, sha)
    try:
        if files:
            git(where, "checkout", head, "--", *files)
        if gone:
            git(where, "rm", "-q", "--ignore-unmatch", "--", *gone)
        yield where
    finally:
        git(repo, "worktree", "remove", "--force", where, check=False)
        shutil.rmtree(where, ignore_errors=True)


def junit(path: str) -> dict[str, str] | None:
    """{id: pass | fail | skip} from a JUnit XML report; None when there is none or it holds no testcase."""
    try:
        root = ET.parse(path).getroot()
    except (OSError, ET.ParseError):
        return None
    ids: dict[str, str] = {}
    for case in root.iter("testcase"):
        cls, name = case.get("classname") or "", case.get("name") or ""
        key = "%s::%s" % (cls, name) if cls and cls != name else name
        kinds = {child.tag for child in case}
        result = "fail" if kinds & {"failure", "error"} else "skip" if "skipped" in kinds else "pass"
        if ids.get(key) != "fail":  # a duplicate id that fails once fails
            ids[key] = result
    return ids or None


def run(where: str, decl: dict, files: list[str], env: dict, label: str, say) -> dict:
    """The declared runner on files in where: {ran, ids, seconds, exit, tail}."""
    report = os.path.join(tempfile.mkdtemp(prefix="test-contract-report-"), "report.xml")
    start = time.monotonic()
    proc = subprocess.Popen([BASH, "-c", decl["run"], "agent-tests", *files], cwd=where,
                            env={**env, "AGENT_TEST_REPORT": report}, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors="replace",
                            start_new_session=True)
    done = threading.Event()

    def beat():  # the SDK's terminal pauses after 30 s without output
        while not done.wait(HEARTBEAT_S):
            say("%s running, %ds" % (label, time.monotonic() - start))
    threading.Thread(target=beat, daemon=True).start()
    try:
        out, _ = proc.communicate(timeout=decl["minutes"] * 60)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        out, _ = proc.communicate()
    finally:
        done.set()
    ids = junit(report)
    shutil.rmtree(os.path.dirname(report), ignore_errors=True)
    return {"ran": ids is not None, "ids": ids or {}, "seconds": round(time.monotonic() - start, 1),
            "exit": proc.returncode, "tail": out.splitlines()[-30:]}


def plan(repo: str, decl: dict, base: str, head: str) -> dict:
    """What the change does to the tests: the selected files, the overlay's inputs, what lies outside."""
    ch = changes(repo, base, head)
    tests = [(s, p) for s, p in ch if under(p, decl["paths"]) and not under(p, decl["runner"])]
    return {
        "changed": [p for _, p in ch],
        "outside": [p for _, p in ch if not under(p, decl["paths"]) or under(p, decl["runner"])],
        "runner": [p for _, p in ch if under(p, decl["runner"])],
        "files": [p for s, p in tests if s != "D"],
        "gone": [p for s, p in tests if s == "D"],
        "selected": sorted(p for s, p in tests if s != "D" and any(fnmatch.fnmatchcase(p, g) for g in decl["select"])),
    }


def at_base(repo: str, decl: dict, base: str, selected: list[str], env: dict, say) -> dict:
    """The base's ids of the selected files that exist there: what a new id is new against."""
    existing = [p for p in selected
                if subprocess.run(["git", "-C", repo, "cat-file", "-e", "%s:%s" % (base, p)], capture_output=True).returncode == 0]
    if not existing:
        return {"ran": True, "ids": {}, "seconds": 0, "exit": 0, "tail": []}
    with tree(repo, base) as where:
        return run(where, decl, existing, env, "base", say)


def result_of(i: str, ids: dict[str, str]) -> str:
    """i's result in ids. A file that fails to load reports one id for the file (pytest's collection
    error, `tests.test_x`): it stands for every id under it, and an id stands for the ids under it."""
    if i in ids:
        return ids[i]
    under_i = [r for k, r in ids.items() if k.startswith(i + "::") or k.startswith(i + ".")]
    if not under_i:
        parent = [r for k, r in ids.items() if r == "fail" and (i.startswith(k + "::") or i.startswith(k + "."))]
        return "fail" if parent else "-"
    return "fail" if "fail" in under_i else "skip" if "skip" in under_i else "pass"


def judge(base_r: dict, over: dict, head_r: dict | None) -> list[dict]:
    """One row per id: its result at the base, the overlay and (in CI) the head, and whether it is new.
    In CI the rows are the head's ids; a load error at the overlay counts against each id under it."""
    names = set(head_r["ids"]) if head_r else set(over["ids"])
    return [{"id": i, "new": result_of(i, base_r["ids"]) == "-", "base": result_of(i, base_r["ids"]),
             "overlay": result_of(i, over["ids"]), "head": head_r["ids"][i] if head_r else None}
            for i in sorted(names)]


def red(repo: str, commit: str, env: dict, say) -> tuple[int, dict]:
    """TF5: every changed path a test path, the runner ran, at the overlay one id fails and no new id passes."""
    base = git(repo, "rev-parse", DEFAULT_BRANCH + "^{commit}").strip()
    decl = declaration(repo, base)
    p = plan(repo, decl, base, commit)
    res = {"base": base, "commit": commit, "files": p["files"], "selected": p["selected"]}
    if not p["changed"]:
        return NOT_OK, {**res, "result": "not_testable", "why": "the commit changes nothing"}
    if p["outside"] or p["gone"]:
        return OUTSIDE, {**res, "result": "contract_outside_tests", "outside": p["outside"] + p["gone"],
                         "why": "a contract adds or changes files under %s only, never the runner's, and deletes none"
                         % ", ".join(decl["paths"])}
    if not p["selected"]:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "no changed file is a test file (%s)" % ", ".join(decl["select"])}
    b = at_base(repo, decl, base, p["selected"], env, say)
    with tree(repo, base, commit, p["files"]) as where:
        o = run(where, decl, p["selected"], env, "overlay", say)
    rows = judge(b, o, None)
    res["rows"] = rows
    if not (b["ran"] and o["ran"]):
        return COULD_NOT_RUN, {**res, "result": "contract_not_red", "why": "the runner could not run", "tail": o["tail"]}
    fails = [r["id"] for r in rows if r["overlay"] == "fail"]
    vacuous = [r["id"] for r in rows if r["new"] and r["overlay"] == "pass"]
    if vacuous:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "new tests pass before any change: %s" % ", ".join(vacuous[:10])}
    if not fails:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "no test fails on the base plus the tests"}
    return OK, {**res, "result": "contract_ready", "ids": fails[:MAX_IDS]}


def ci(repo: str, base: str, head: str, env: dict, say) -> tuple[int, dict]:
    """TF7: the runner ran both times, no new id passes at the overlay, every id passes at the head
    (a skip is no pass), one id fails at the overlay and passes at the head, no runner file changed."""
    decl = declaration(repo, base)
    p = plan(repo, decl, base, head)
    res = {"base": base, "head": head, "selected": p["selected"], "runner": p["runner"]}
    if p["runner"]:
        return NOT_OK, {**res, "result": "red", "why": ["the runner's own files changed: %s" % ", ".join(p["runner"])]}
    if not p["selected"]:
        return OK, {**res, "result": "green", "why": ["no changed test file"]}
    b = at_base(repo, decl, base, p["selected"], env, say)
    with tree(repo, base, head, p["files"], p["gone"]) as where:
        o = run(where, decl, p["selected"], env, "overlay", say)
    with tree(repo, head) as where:
        h = run(where, decl, p["selected"], env, "head", say)
    rows = judge(b, o, h)
    res["rows"] = rows
    why = []
    if not (b["ran"] and o["ran"] and h["ran"]):
        why.append("the runner could not run (%s)" % ", ".join(n for n, x in (("base", b), ("overlay", o), ("head", h)) if not x["ran"]))
    else:
        why += ["new test passes on the base plus the tests: %s" % r["id"] for r in rows if r["new"] and r["overlay"] == "pass"]
        why += ["does not pass at the head (%s): %s" % (r["head"], r["id"]) for r in rows if r["head"] != "pass"]
        if not any(r["overlay"] == "fail" and r["head"] == "pass" for r in rows):
            why.append("no test fails on the base plus the tests and passes at the head")
    return (NOT_OK if why else OK), {**res, "result": "red" if why else "green", "why": why or ["fail-to-pass holds"]}


def contract_of(repo: str, base: str) -> dict | None:
    """The contract commit on the branch: the first commit since base the tester's harness signed."""
    for sha in git(repo, "rev-list", "--reverse", "%s..HEAD" % base).split():
        msg = git(repo, "show", "-s", "--format=%B", sha)
        trailers = subprocess.run(["git", "-C", repo, "interpret-trailers", "--parse", "--no-divider"],
                                  input=msg, capture_output=True, text=True).stdout.splitlines()
        values = {}
        for t in trailers:
            key, _, value = t.partition(":")
            values.setdefault(key, []).append(value.strip())
        if values.get(TRAILER_RUN):
            return {"commit": sha, "run": values[TRAILER_RUN][0], "files": values.get(TRAILER_FILE, []),
                    "ids": values.get(TRAILER_ID, [])}
    return None


def hold(repo: str, env: dict, say) -> tuple[int, dict]:
    """TF6: the contract's tests at HEAD, on a clean checkout: every contract id passes."""
    base = git(repo, "rev-parse", DEFAULT_BRANCH + "^{commit}").strip()
    c = contract_of(repo, base)
    if not c:
        return UNDECLARED, {"result": "no_contract"}
    decl = declaration(repo, base)
    head = git(repo, "rev-parse", "HEAD").strip()
    selected = sorted(p for p in c["files"] if any(fnmatch.fnmatchcase(p, g) for g in decl["select"]))
    with tree(repo, head) as where:
        h = run(where, decl, selected, env, "contract", say)
    if not h["ran"]:
        return COULD_NOT_RUN, {**c, "head": head, "result": "could_not_run", "tail": h["tail"]}
    failing = sorted(i for i in c["ids"] if result_of(i, h["ids"]) != "pass")
    return (NOT_OK if failing else OK), {**c, "head": head, "result": "red" if failing else "green", "failing": failing}


def summary(res: dict) -> str:
    """The check run's summary: each id's results, the reasons."""
    lines = ["### Test contract: %s" % res["result"], ""]
    lines += ["- %s" % w for w in res.get("why", [])]
    if res.get("rows"):
        lines += ["", "| id | new | base | base + tests | head |", "|---|---|---|---|---|"]
        lines += ["| `%s` | %s | %s | %s | %s |" % (r["id"], "yes" if r["new"] else "", r["base"], r["overlay"], r["head"] or "")
                  for r in res["rows"]]
    return "\n".join(lines) + "\n"


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="test-contract")
    sub = ap.add_subparsers(dest="mode", required=True)
    r = sub.add_parser("red")
    r.add_argument("--commit", default="HEAD")
    sub.add_parser("hold")
    c = sub.add_parser("ci")
    c.add_argument("--base", required=True)
    c.add_argument("--head", required=True)
    c.add_argument("--summary")
    ap.add_argument("--repo", default=os.environ.get("AGENT_CHECKS_REPO", "/workspace/repo"))
    ap.add_argument("--json", help="write the result here")
    args = ap.parse_args(argv)

    def say(m):
        print("test-contract: " + m, file=sys.stderr, flush=True)
    env = dict(os.environ)
    try:
        if args.mode == "red":
            code, res = red(args.repo, git(args.repo, "rev-parse", args.commit).strip(), env, say)
        elif args.mode == "hold":
            code, res = hold(args.repo, env, say)
        else:
            code, res = ci(args.repo, args.base, args.head, env, say)
    except Undeclared as exc:
        say(str(exc))
        return UNDECLARED
    if args.json:
        with open(args.json, "w") as f:
            json.dump(res, f, indent=1)
    if args.mode == "ci" and args.summary:
        with open(args.summary, "a") as f:
            f.write(summary(res))
    print(json.dumps({k: v for k, v in res.items() if k != "tail"}, indent=1))
    return code


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
