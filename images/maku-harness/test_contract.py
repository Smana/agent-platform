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
import collections
import contextlib
import fnmatch
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import xml.etree.ElementTree as ET

import yaml

import agent_checks  # beside this script: its git_env() and guards() harden every git call here

DECLARATION = ".github/agent-checks.yaml"
DEFAULT_BRANCH = "refs/remotes/origin/HEAD"
DEFAULT_MINUTES, MAX_MINUTES = 5, 15
HEARTBEAT_S = 15
MAX_IDS = 50
# Bounds on what a runner can hand back: the report's size and testcases, the output kept, the grace after a kill.
MAX_REPORT_BYTES, MAX_CASES, MAX_OUTPUT, KILL_GRACE_S = 10 * 1024 * 1024, 20000, 64 * 1024, 5
# Exit codes, the harness's ends (TF5) and the job's conclusion (TF7).
OK, NOT_OK, UNDECLARED, OUTSIDE, COULD_NOT_RUN = 0, 1, 2, 3, 4
# hold fails closed: a missing, id-less or file-less contract is each its own result, never UNDECLARED.
NO_CONTRACT, NO_IDS, NO_FILES, NOT_ON_BASE, DECLARATION_LOST = 5, 6, 7, 8, 9
# The runner's shell, resolved from the caller's PATH: a step's PATH is the toolchain's.
BASH = shutil.which("bash") or "/bin/bash"
TRAILER_RUN, TRAILER_FILE, TRAILER_ID = "Agent-Contract", "Agent-Contract-File", "Agent-Contract-Id"


class Undeclared(Exception):
    """The base declares no tests: exit 2."""


class BadRef(Exception):
    """A base, head or commit that is no commit of the repository."""


def git_cmd(repo: str, *args: str) -> list[str]:
    """git with the repository's own config and the agent's refs/replace neutralised (agent_checks'
    guards), and every pathspec literal: a PR's file name is never a glob or a magic pathspec."""
    return ["git", "--literal-pathspecs", "-C", repo, *agent_checks.guards(repo), *args]


def git(repo: str, *args: str, check: bool = True) -> str:
    out = subprocess.run(git_cmd(repo, *args), capture_output=True, text=True, env=agent_checks.git_env())
    if check and out.returncode:
        raise RuntimeError("git %s: %s" % (" ".join(args), out.stderr.strip()))
    return out.stdout


def resolve(repo: str, ref: str) -> str:
    """ref as a commit sha; refused when it is none, or an option."""
    out = subprocess.run(git_cmd(repo, "rev-parse", "--verify", "--end-of-options", "%s^{commit}" % ref),
                         capture_output=True, text=True, env=agent_checks.git_env())
    if out.returncode or not out.stdout.strip():
        raise BadRef("%r is no commit of the repository" % ref)
    return out.stdout.strip()


def base_of(repo: str, base: str | None) -> tuple[str, str]:
    """(sha, source): the harness's base when given, else the local origin/HEAD."""
    if base:
        return resolve(repo, base), "arguments"
    return resolve(repo, DEFAULT_BRANCH), "origin/HEAD"


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
    need(isinstance(m, int) and not isinstance(m, bool) and 1 <= m <= MAX_MINUTES, "timeoutMinutes must be 1 to %d" % MAX_MINUTES)
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
    """{id: pass | fail | skip} from a JUnit XML report; None when there is none, it is over MAX_REPORT_BYTES or
    MAX_CASES testcases, or it holds no testcase."""
    try:
        if os.path.getsize(path) > MAX_REPORT_BYTES:
            return None
        ids: dict[str, str] = {}
        seen = 0
        for _, case in ET.iterparse(path):  # end events: a testcase's children are in
            if case.tag != "testcase":
                continue
            seen += 1
            if seen > MAX_CASES:
                return None
            cls, name = case.get("classname") or "", case.get("name") or ""
            key = "%s::%s" % (cls, name) if cls and cls != name else name
            kinds = {child.tag for child in case}
            result = "fail" if kinds & {"failure", "error"} else "skip" if "skipped" in kinds else "pass"
            if ids.get(key) != "fail":  # a duplicate id that fails once fails
                ids[key] = result
            case.clear()
    except (OSError, ET.ParseError):
        return None
    return ids or None


def run(where: str, decl: dict, files: list[str], env: dict, label: str, say) -> dict:
    """The declared runner on files in where: {ran, timeout, ids, seconds, exit, tail}. A timeout is
    not a run; the output kept is the last MAX_OUTPUT bytes."""
    report = os.path.join(tempfile.mkdtemp(prefix="test-contract-report-"), "report.xml")
    start = time.monotonic()
    proc = subprocess.Popen([BASH, "-c", decl["run"], "agent-tests", *files], cwd=where,
                            env={**env, "AGENT_TEST_REPORT": report}, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    done = threading.Event()
    kept = collections.deque()  # (bytes) chunks, trimmed to MAX_OUTPUT
    size = [0]
    lock = threading.Lock()

    def beat():  # the SDK's terminal pauses after 30 s without output
        while not done.wait(HEARTBEAT_S):
            say("%s running, %ds" % (label, time.monotonic() - start))

    def drain():
        try:
            while chunk := proc.stdout.read1(65536):
                with lock:
                    kept.append(chunk)
                    size[0] += len(chunk)
                    while size[0] > MAX_OUTPUT and len(kept) > 1:
                        size[0] -= len(kept.popleft())
        except (OSError, ValueError):
            pass
    threading.Thread(target=beat, daemon=True).start()
    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    timed_out = False
    try:
        proc.wait(timeout=decl["minutes"] * 60)
    except subprocess.TimeoutExpired:
        timed_out = True
    finally:
        done.set()
    try:  # the group, whatever the end: a straggler must not outlive the run or hold the pipe
        os.killpg(proc.pid, signal.SIGKILL)
    except OSError:
        pass
    try:
        proc.wait(timeout=KILL_GRACE_S)
    except subprocess.TimeoutExpired:
        pass
    # A descendant that left the group (setsid) may hold the pipe: the reader is a daemon, left blocked
    # in read1 holding the buffer's lock, so the stream is never closed from here. What it kept so far stands.
    reader.join(KILL_GRACE_S)
    ids = None if timed_out else junit(report)
    shutil.rmtree(os.path.dirname(report), ignore_errors=True)
    with lock:
        out = b"".join(kept)[-MAX_OUTPUT:].decode(errors="replace")
    return {"ran": ids is not None, "timeout": timed_out, "ids": ids or {}, "seconds": round(time.monotonic() - start, 1),
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
                if subprocess.run(git_cmd(repo, "cat-file", "-e", "%s:%s" % (base, p)), capture_output=True,
                                  env=agent_checks.git_env()).returncode == 0]
    if not existing:
        return {"ran": True, "timeout": False, "ids": {}, "seconds": 0, "exit": 0, "tail": []}
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


DOC_REF = re.compile(r"[\w./-]*\.(?:md|mdx|markdown|rst|txt)\b", re.I)
CODE_REF = re.compile(r"[\w./-]+\.(?:sh|bash|py|go|js|mjs|ts|rb|yaml|yml|json|toml|tf|cue|rs|java)\b", re.I)
PATH_REF = re.compile(r"(?<![\w:/.-])[\w.-]+(?:/[\w.-]+)+")
PY_IMPORT = re.compile(r"^\s*(?:import|from)\s+([A-Za-z_]\w*)", re.M)


def reads_only_docs(repo: str, commit: str, files: list[str]) -> bool:
    """A contract whose every file only greps or reads documentation: it names a *.md file, and no code
    file (a script, a module) other than itself, and imports nothing outside the standard library. Such a
    test pins prose, not behaviour: the change is not testable."""
    if not files:
        return False
    for f in files:
        try:
            text = git(repo, "show", "%s:%s" % (commit, f))
        except Exception:  # noqa: BLE001 -- unreadable: no verdict
            return False
        own = os.path.basename(f)
        refs = {os.path.basename(m.group(0)) for m in CODE_REF.finditer(text)} - {own}
        refs |= {m.group(0) for m in PATH_REF.finditer(text) if not DOC_REF.fullmatch(m.group(0)) and m.group(0) != f}
        mods = set(PY_IMPORT.findall(text)) - set(sys.stdlib_module_names) - {"pytest", "unittest"}
        if not DOC_REF.search(text) or refs or mods:
            return False
    return True


def red(repo: str, commit: str, env: dict, say, base: str | None = None) -> tuple[int, dict]:
    """TF5: every changed path a test path, the runner ran, at the overlay one id fails and no new id passes.
    base is the harness's (the sha it read at startup); without it, the local origin/HEAD."""
    base, source = base_of(repo, base)
    commit = resolve(repo, commit)
    decl = declaration(repo, base)
    p = plan(repo, decl, base, commit)
    res = {"base": base, "base_source": source, "commit": commit, "files": p["files"], "selected": p["selected"]}
    if not p["changed"]:
        return NOT_OK, {**res, "result": "not_testable", "why": "the commit changes nothing"}
    if p["outside"] or p["gone"]:
        return OUTSIDE, {**res, "result": "contract_outside_tests", "outside": p["outside"] + p["gone"],
                         "why": "a contract adds or changes files under %s only, never the runner's, and deletes none"
                         % ", ".join(decl["paths"])}
    if not p["selected"]:
        return NOT_OK, {**res, "result": "contract_not_red", "why": "no changed file is a test file (%s)" % ", ".join(decl["select"])}
    if reads_only_docs(repo, commit, p["selected"]):
        return NOT_OK, {**res, "result": "not_testable",
                        "why": "the contract only greps or reads documentation: a change with no behaviour is not testable"}
    b = at_base(repo, decl, base, p["selected"], env, say)
    with tree(repo, base, commit, p["files"]) as where:
        o = run(where, decl, p["selected"], env, "overlay", say)
    rows = judge(b, o, None)
    res["rows"] = rows
    if not (b["ran"] and o["ran"]):
        why = "the runner could not run" + (" (timed out)" if b["timeout"] or o["timeout"] else "")
        return COULD_NOT_RUN, {**res, "result": "contract_not_red", "why": why, "tail": o["tail"]}
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
    base, head = resolve(repo, base), resolve(repo, head)
    decl = declaration(repo, base)
    p = plan(repo, decl, base, head)
    res = {"base": base, "head": head, "selected": p["selected"], "runner": p["runner"]}
    if p["runner"]:
        return NOT_OK, {**res, "result": "red", "why": ["the runner's own files changed: %s" % ", ".join(p["runner"])]}
    note = ["deleted test files (no verdict on them): %s" % ", ".join(p["gone"])] if p["gone"] else []
    if not p["selected"]:
        if p["gone"]:  # green, reporting none (TF7), with the deletions named
            return OK, {**res, "result": "green", "why": ["no test added or changed"] + note}
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
        why.append("the runner could not run (%s)" % ", ".join(
            n + (" timed out" if x["timeout"] else "") for n, x in (("base", b), ("overlay", o), ("head", h)) if not x["ran"]))
    else:
        why += ["new test passes on the base plus the tests: %s" % r["id"] for r in rows if r["new"] and r["overlay"] == "pass"]
        why += ["does not pass at the head (%s): %s" % (r["head"], r["id"]) for r in rows if r["head"] != "pass"]
        if not any(r["overlay"] == "fail" and r["head"] == "pass" for r in rows):
            why.append("no test fails on the base plus the tests and passes at the head")
    return (NOT_OK if why else OK), {**res, "result": "red" if why else "green", "why": (why or ["fail-to-pass holds"]) + note}


def contract_at(repo: str, sha: str) -> dict | None:
    """The contract trailers of one commit; None when it signs none."""
    msg = git(repo, "show", "-s", "--format=%B", sha)
    trailers = subprocess.run(["git", "interpret-trailers", "--parse", "--no-divider"],
                              input=msg, capture_output=True, text=True).stdout.splitlines()
    values: dict[str, list[str]] = {}
    for t in trailers:
        key, _, value = t.partition(":")
        values.setdefault(key, []).append(value.strip())
    if values.get(TRAILER_RUN):
        return {"commit": sha, "run": values[TRAILER_RUN][0], "files": values.get(TRAILER_FILE, []),
                "ids": values.get(TRAILER_ID, [])}
    return None


def contract_of(repo: str, base: str) -> dict | None:
    """The contract commit on the branch: the first commit since base the tester's harness signed."""
    for sha in git(repo, "rev-list", "--reverse", "%s..HEAD" % base).split():
        found = contract_at(repo, sha)
        if found:
            return found
    return None


def hold(repo: str, env: dict, say, base: str | None = None, contract: str | None = None) -> tuple[int, dict]:
    """TF6: the contract's tests at HEAD, on a clean checkout: every contract id passes, and neither a
    contract file nor a runner file differs from the contract commit. Fails closed. The harness passes the
    base and the contract commit it recorded; without them, the local origin/HEAD and the trailer search."""
    base, bsource = base_of(repo, base)
    def ancestor(a: str, b: str) -> bool:
        return subprocess.run(git_cmd(repo, "merge-base", "--is-ancestor", a, b), env=agent_checks.git_env()).returncode == 0
    if contract:
        contract = resolve(repo, contract)
        source = "arguments"
        if contract == base or not ancestor(base, contract):  # the base strictly under the contract commit
            return NOT_ON_BASE, {"base": base, "base_source": bsource, "source": source, "commit": contract,
                                 "result": "contract_not_on_base"}
        c = contract_at(repo, contract) if ancestor(contract, "HEAD") else None
    else:
        c = contract_of(repo, base)
        source = "origin/HEAD and the trailer search"
    res = {"base": base, "base_source": bsource, "source": source}
    if not c:
        return NO_CONTRACT, {**res, "result": "no_contract"}
    res.update(c)
    if not c["ids"]:
        return NO_IDS, {**res, "result": "contract_no_ids"}
    try:
        decl = declaration(repo, base)
    except Undeclared as exc:  # a contract exists: never exit 2, which reads "off"
        return DECLARATION_LOST, {**res, "result": "contract_declaration_lost", "why": str(exc)}
    selected = sorted(p for p in c["files"] if any(fnmatch.fnmatchcase(p, g) for g in decl["select"]))
    if not selected:
        return NO_FILES, {**res, "result": "contract_no_files"}
    head = git(repo, "rev-parse", "HEAD").strip()
    res["head"] = head
    touched = [x for x in git(repo, "diff-tree", "--root", "--no-commit-id", "-r", "-z", "--name-only", "--no-renames",
                              c["commit"]).split("\0") if x and under(x, decl["paths"])]
    watched = sorted(set(c["files"]) | set(touched) | set(decl["runner"]))
    modified = [x for x in git(repo, "diff", "--name-only", "-z", c["commit"], head, "--", *watched).split("\0") if x]
    if modified:  # the contract is the tests as the tester wrote them, under the runner as the base has it
        return NOT_OK, {**res, "result": "contract_modified", "modified": modified, "failing": []}
    with tree(repo, head) as where:
        h = run(where, decl, selected, env, "contract", say)
    if not h["ran"]:
        return COULD_NOT_RUN, {**res, "result": "could_not_run", "tail": h["tail"]}
    failing = sorted(i for i in c["ids"] if result_of(i, h["ids"]) != "pass")
    return (NOT_OK if failing else OK), {**res, "result": "red" if failing else "green", "failing": failing}


def esc(text) -> str:
    """Text for a check run summary (markdown): no markup, table break or HTML from an id or a reason."""
    text = " ".join(str(text).split()).replace("`", "'").replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    return "".join("\\" + ch if ch in "|*_[]\\" else ch for ch in text)


def summary(res: dict) -> str:
    """The check run's summary: each id's results, the reasons."""
    lines = ["### Test contract: %s" % esc(res["result"]), ""]
    lines += ["- %s" % esc(w) for w in res.get("why", [])]
    if res.get("rows"):
        lines += ["", "| id | new | base | base + tests | head |", "|---|---|---|---|---|"]
        lines += ["| %s | %s | %s | %s | %s |" % (esc(r["id"]), "yes" if r["new"] else "", r["base"], r["overlay"], r["head"] or "")
                  for r in res["rows"]]
    return "\n".join(lines) + "\n"


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="test-contract")
    sub = ap.add_subparsers(dest="mode", required=True)
    r = sub.add_parser("red")
    r.add_argument("--commit", default="HEAD")
    r.add_argument("--base", help="the base sha the harness recorded; default the local origin/HEAD")
    h = sub.add_parser("hold")
    h.add_argument("--base", help="the base sha the harness recorded; default the local origin/HEAD")
    h.add_argument("--contract", help="the contract commit the harness recorded; default the trailer search")
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
            code, res = red(args.repo, args.commit, env, say, base=args.base)
        elif args.mode == "hold":
            code, res = hold(args.repo, env, say, base=args.base, contract=args.contract)
        else:
            code, res = ci(args.repo, args.base, args.head, env, say)
    except Undeclared as exc:
        say(str(exc))
        return UNDECLARED
    except (BadRef, RuntimeError) as exc:
        say(str(exc))
        return COULD_NOT_RUN
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
