#!/agent-server/.venv/bin/python
"""agent-checks: the repository's fast checks, the CI's fast subset, run in the sandbox (v3 feature 3).

The repository declares them in .github/agent-checks.yaml, read from the clone's default branch
(refs/remotes/origin/HEAD), never from the agent's branch (RC2). The tools it lists are installed by
mise at the versions the work tree's mise.toml pins, into /workspace/.toolchain (RC1). Each step is a
`bash -c` line in /workspace/repo, in its own process group, with the agent's environment minus every
credential path (RC5). The result is evidence for the agent, the judge and the reviewer, never a
gate: the sandbox is the agent's to change, and the CI stays the only judge of a merge.

  agent-checks                every step; exit 0 when none failed, 1 otherwise, 2 when nothing is declared
  agent-checks --step NAME    one step; its result never stands for the head's
  agent-checks --install      the toolchain only: agent-run's background prefetch
"""
import argparse
import collections
import fcntl
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
import tomllib

import yaml

REPO = os.environ.get("AGENT_CHECKS_REPO", "/workspace/repo")
STATE = os.environ.get("AGENT_CHECKS_DIR", "/workspace/.agent-checks")
TOOLCHAIN = os.environ.get("AGENT_CHECKS_TOOLCHAIN", "/workspace/.toolchain")
MISE = os.environ.get("AGENT_CHECKS_MISE", "/opt/agent/bin/mise")
DECLARATION = ".github/agent-checks.yaml"
DEFAULT_BRANCH = "refs/remotes/origin/HEAD"
# RC9: the install's bound; the declaration's default and ceiling, in minutes of MINUTE seconds.
MINUTE = 60
INSTALL_S = 300
DEFAULT_MINUTES, MAX_MINUTES = 8, 15
# RC4: a line at least every 20 s keeps the terminal's 30 s no-output pause away (SDK 1.49.6,
# NO_CHANGE_TIMEOUT_SECONDS); the summary's cap; each failed step's tail.
HEARTBEAT_S = 15
SUMMARY_CAP = 8000
TAIL_LINES = 60
KILL_GRACE_S = 5
STEP_NAME = re.compile(r"^[a-z0-9][a-z0-9-]{0,39}$")
# RC5: what a step never inherits. CI=true turns a missing-Docker skip into a failure.
SCRUBBED = {"CI", "GITHUB_ACTIONS", "GH_TOKEN", "GITHUB_TOKEN", "GIT_TOKEN_FILE", "GIT_TOKEN_CACHE",
            "LLM_API_KEY_FILE", "LLM_API_KEY", "OH_SECRET_KEY", "SESSION_API_KEY", "STS_URL"}
SCRUBBED_PREFIXES = ("OH_SESSION_API_KEYS_", "MISE_")
# A step's output: the log's byte cap, then one marker line; each kept tail line's length.
LOG_CAP, LINE_CAP = 50 * 1024 * 1024, 4096
RESULTS = ("pass", "fail", "timeout", "skip")
LEDGER = "runs.jsonl"


class ToolchainError(Exception):
    """mise answered with something agent-checks cannot use: exit 1."""


class Undeclared(Exception):
    """The repository declares nothing usable: exit 2."""


def git_env() -> dict:
    """refs/replace is the agent's to write: no replacement object may stand in for a blob or a commit."""
    return {**os.environ, "GIT_NO_REPLACE_OBJECTS": "1"}


# The repository's own config and hooks are the agent's to write: nothing it names may run during
# agent-checks' git calls (fsmonitor, hooks, external diff drivers, textconv, clean/smudge filters).
GUARDS = ["-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external="]


def guards() -> list[str]:
    out = subprocess.run(["git", "-C", REPO, *GUARDS, "config", "--get-regexp", r"^filter\..*\.(clean|smudge)$"],
                         capture_output=True, text=True, env=git_env()).stdout
    names = {line.split()[0].rsplit(".", 1)[0] for line in out.splitlines() if line.strip()}
    return GUARDS + [a for n in sorted(names) for a in ("-c", n + ".clean=cat", "-c", n + ".smudge=cat")]


def git(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["git", "-C", REPO, *guards(), *args], capture_output=True, text=True, env=git_env())


def validate(doc) -> dict:
    """The declaration, checked as each repository's coverage suite checks it; Undeclared names the
    first problem."""
    def need(cond, why):
        if not cond:
            raise Undeclared("%s: %s" % (DECLARATION, why))
    need(isinstance(doc, dict), "not a mapping")
    extra = set(doc) - {"version", "timeoutMinutes", "tools", "definedBy", "steps", "ciOnly"}
    need(not extra, "unknown keys %s" % sorted(extra))
    need(doc.get("version") == 1, "version must be 1")
    minutes = doc.get("timeoutMinutes", DEFAULT_MINUTES)
    need(isinstance(minutes, int) and not isinstance(minutes, bool) and 1 <= minutes <= MAX_MINUTES, "timeoutMinutes must be 1 to %d" % MAX_MINUTES)
    for key in ("tools", "definedBy"):
        value = doc.get(key, [])
        need(isinstance(value, list) and all(isinstance(t, str) and t for t in value), "%s must be a list of strings" % key)
    steps = doc.get("steps")
    need(isinstance(steps, list) and steps, "steps must be a non-empty list")
    names = set()
    for s in steps:
        need(isinstance(s, dict) and not set(s) - {"name", "run", "ci", "timeoutMinutes", "stopOnFailure"},
             "a step takes name, run, ci, timeoutMinutes and stopOnFailure")
        need(isinstance(s.get("name"), str) and STEP_NAME.match(s["name"]) and s["name"] not in names,
             "step names are unique and match %s" % STEP_NAME.pattern)
        names.add(s["name"])
        need(isinstance(s.get("run"), str) and s["run"].strip(), "step %s has no run" % s["name"])
        need(isinstance(s.get("ci"), str) and s["ci"], "step %s names no ci check" % s["name"])
        t = s.get("timeoutMinutes", minutes)
        need(isinstance(t, int) and not isinstance(t, bool) and 1 <= t <= minutes, "step %s: timeoutMinutes must be 1 to %d" % (s["name"], minutes))
        need(isinstance(s.get("stopOnFailure", False), bool), "step %s: stopOnFailure is a boolean" % s["name"])
    ci_only = doc.get("ciOnly", [])
    need(isinstance(ci_only, list) and all(isinstance(c, dict) and set(c) == {"check", "part", "why"} for c in ci_only),
         "every ciOnly entry has exactly check, part and why")
    return {"minutes": minutes, "tools": doc.get("tools", []), "definedBy": doc.get("definedBy", []),
            "steps": steps, "ciOnly": ci_only}


def declaration() -> tuple[str, str, dict]:
    """(default branch commit, declaration blob, declaration), from the clone's default branch."""
    # AGENT_CHECKS_BASE is the commit agent-run read at startup, before the agent could move the ref.
    ref = os.environ.get("AGENT_CHECKS_BASE") or DEFAULT_BRANCH
    base = git("rev-parse", "--verify", "--quiet", ref + "^{commit}").stdout.strip()
    if not base:
        raise Undeclared("the clone has no default branch (%s)" % DEFAULT_BRANCH)
    blob = git("rev-parse", "--verify", "--quiet", "%s:%s" % (base, DECLARATION)).stdout.strip()
    if not blob:
        raise Undeclared("the default branch declares no checks (%s)" % DECLARATION)
    try:
        raw = subprocess.run(["git", "-C", REPO, *GUARDS, "cat-file", "blob", blob], capture_output=True, env=git_env()).stdout
        doc = yaml.safe_load(raw.decode("utf-8"))
    except (yaml.YAMLError, UnicodeDecodeError) as exc:
        raise Undeclared("%s: %s" % (DECLARATION, exc)) from exc
    return base, blob, validate(doc)


def pins(tools: list[str]) -> dict[str, str | None]:
    """Each declared tool's version in the work tree's mise.toml, the branch's pins (a Renovate bump
    included); None for a tool the file does not pin."""
    try:
        with open(os.path.join(REPO, "mise.toml"), "rb") as f:
            table = tomllib.load(f).get("tools", {})
    except (OSError, tomllib.TOMLDecodeError):
        table = {}
    out = {}
    for t in tools:
        v = table.get(t)
        v = v.get("version") if isinstance(v, dict) else v
        out[t] = v if isinstance(v, str) and v else None
    return out


def mise_env(token: str = "") -> dict:
    """mise's own environment: every directory under the toolchain, no global or project config
    read, no versions host (measured: 58 refused connections an install), and the run's GitHub
    token for its API calls when one is given: the install only, never a step."""
    env = {"HOME": os.environ.get("HOME", "/tmp"), "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
           "MISE_DATA_DIR": os.path.join(TOOLCHAIN, "mise"), "MISE_CACHE_DIR": os.path.join(TOOLCHAIN, "mise-cache"),
           "MISE_STATE_DIR": os.path.join(TOOLCHAIN, "mise-state"), "MISE_CONFIG_DIR": os.path.join(TOOLCHAIN, "mise-config"),
           "MISE_GLOBAL_CONFIG_FILE": os.path.join(TOOLCHAIN, "mise-config", "none.toml"),
           "MISE_USE_VERSIONS_HOST": "0", "MISE_YES": "1"}
    for k in ("HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "MISE_LOG_LEVEL"):
        if k in os.environ:
            env[k] = os.environ[k]
    if token:
        env["GITHUB_TOKEN"] = token
    return env


def github_token() -> str:
    """The run's token from git's credential helper (git-credential-agent), or "" when there is none:
    mise's GitHub API calls then go unauthenticated."""
    try:
        out = subprocess.run(["git", "credential", "fill"], input="protocol=https\nhost=github.com\n\n",
                             capture_output=True, text=True, timeout=20,
                             env={**os.environ, "GIT_TERMINAL_PROMPT": "0"}).stdout
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return next((line[9:] for line in out.splitlines() if line.startswith("password=")), "")


def installed(specs: dict[str, str]) -> set[str]:
    """The tools whose spec mise already holds."""
    env = {**mise_env(), "MISE_OFFLINE": "1"}
    return {t for t, s in specs.items()
            if subprocess.run([MISE, "where", s], cwd=TOOLCHAIN, env=env, capture_output=True).returncode == 0}


def wait(proc, limit: float, beat) -> str:
    """Waits for proc at most limit seconds, calling beat(elapsed) every HEARTBEAT_S. On overrun its
    whole process group gets SIGTERM, then SIGKILL. "done" or "timeout"."""
    start = time.monotonic()
    while True:
        left = limit - (time.monotonic() - start)
        try:
            proc.wait(timeout=max(0.0, min(HEARTBEAT_S, left)))
            return "done"
        except subprocess.TimeoutExpired:
            pass
        if time.monotonic() - start >= limit:
            for sig in (signal.SIGTERM, signal.SIGKILL):
                try:
                    os.killpg(proc.pid, sig)
                except ProcessLookupError:
                    break
                try:
                    proc.wait(timeout=KILL_GRACE_S)
                    break
                except subprocess.TimeoutExpired:
                    continue
            return "timeout"
        beat(int(time.monotonic() - start))


def install(tools: dict[str, str | None], say) -> list[str]:
    """Installs the pinned tools mise lacks, within INSTALL_S; returns the declared tools still missing."""
    os.makedirs(TOOLCHAIN, exist_ok=True)
    specs = {t: "%s@%s" % (t, v) for t, v in tools.items() if v}
    have = installed(specs)
    todo = [s for t, s in specs.items() if t not in have]
    if todo:
        say("installing %s" % " ".join(todo))
        with open(os.path.join(STATE, "install.log"), "a") as log:
            proc = subprocess.Popen([MISE, "install", *todo], cwd=TOOLCHAIN, env=mise_env(github_token()),
                                    stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            if wait(proc, INSTALL_S, lambda s: say("installing, %ds" % s)) == "timeout":
                say("the install stopped after %ds" % INSTALL_S)
        have = installed(specs)
    return [t for t in tools if t not in have]


def step_env(tools: dict[str, str | None], missing: list[str]) -> dict:
    """The agent's environment, scrubbed (RC5: the environment only; a step runs as the agent's own
    user and no isolation is claimed), with the installed tools first on PATH as `mise env`
    gives them (PATH, GOROOT, GOBIN: what the CI's mise-action exports), and the caches under the
    toolchain: the 10 Gi workspace, kept by a suspendable run's PVC, not HOME's 2 Gi."""
    env = {k: v for k, v in os.environ.items() if k not in SCRUBBED and not k.startswith(SCRUBBED_PREFIXES)}
    env.setdefault("AI_AGENT", "openhands")
    specs = ["%s@%s" % (t, v) for t, v in tools.items() if v and t not in missing]
    if specs:
        out = subprocess.run([MISE, "env", "--json", *specs], cwd=TOOLCHAIN, capture_output=True, text=True,
                             env={**mise_env(), "MISE_OFFLINE": "1", "PATH": env.get("PATH", "/usr/bin:/bin")})
        if out.returncode == 0:
            try:
                exported = json.loads(out.stdout)
            except ValueError as exc:
                raise ToolchainError("mise env printed no JSON object") from exc
            if not isinstance(exported, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in exported.items()):
                raise ToolchainError("mise env printed a value that is not a string")
            env.update(exported)
    env["XDG_CACHE_HOME"] = os.path.join(TOOLCHAIN, "cache")
    env["GOPATH"] = os.path.join(TOOLCHAIN, "go")
    return env


def run_step(step: dict, env: dict, limit: float, log, say) -> dict:
    """One step in its own process group; the full output to log, the last TAIL_LINES kept."""
    say("%s: %s" % (step["name"], step["run"]))
    log.write("\n=== %s: %s\n" % (step["name"], step["run"]))
    log.flush()
    tail = collections.deque(maxlen=TAIL_LINES)
    start = time.monotonic()
    proc = subprocess.Popen(["bash", "-c", step["run"]], cwd=REPO, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True,
                            text=True, errors="replace")

    def drain():
        written = 0
        for line in iter(lambda: proc.stdout.readline(LINE_CAP), ""):  # a newline-less flood arrives in chunks
            if written < LOG_CAP:
                data = line.encode("utf-8", "replace")
                if written + len(data) > LOG_CAP:
                    data = data[:LOG_CAP - written]
                log.write(data.decode("utf-8", "ignore"))
                written += len(data)
                if written >= LOG_CAP:
                    log.write("\n=== output truncated at %d bytes\n" % LOG_CAP)
                    written = LOG_CAP + 1
            tail.append(line.rstrip("\n")[-LINE_CAP:])
        log.flush()
    reader = threading.Thread(target=drain, daemon=True)
    reader.start()
    ended = wait(proc, limit, lambda s: say("%s running, %ds" % (step["name"], s)))
    reader.join(KILL_GRACE_S)
    if not reader.is_alive():
        proc.stdout.close()
    seconds = round(time.monotonic() - start, 1)
    result = "timeout" if ended == "timeout" else ("pass" if proc.returncode == 0 else "fail")
    say("%s: %s in %ss" % (step["name"], result, seconds))
    return {"name": step["name"], "ci": step["ci"], "result": result, "seconds": seconds,
            "exit": proc.returncode, "tail": list(tail) if result != "pass" else []}


def changed() -> set[str]:
    """Tracked files the work tree changes against HEAD."""
    return set(git("diff", "--no-ext-diff", "--no-textconv", "--name-only", "HEAD").stdout.split())


def touched(base: str, defined_by: list[str]) -> list[str]:
    """The definedBy paths the branch changes against the default branch, committed or not."""
    fork = git("merge-base", base, "HEAD").stdout.strip() or base
    names = set(git("diff", "--no-ext-diff", "--no-textconv", "--name-only", fork, "HEAD").stdout.split()) | changed()
    return sorted(n for n in names if any(n == p or (p.endswith("/") and n.startswith(p)) for p in defined_by))


def summary(res: dict) -> str:
    """At most SUMMARY_CAP characters: the counts, a line per step, each failed step's tail, what
    stays CI-only, the full log's path. Tails give way first."""
    counts = collections.Counter(s["result"] for s in res["steps"])
    head = ["agent-checks %s (declaration %s): %s in %ss" % (
        res["head"][:12], res["declaration"][:12],
        ", ".join("%d %s" % (counts[r], r) for r in RESULTS if counts[r]), res["seconds"])]
    if res["dirty"]:
        head.append("the work tree had uncommitted changes: this is not %s's result alone" % res["head"][:12])
    if res["changedByChecks"]:
        head.append("the checks changed these files: %s (review them, then commit or restore them)"
                    % ", ".join(res["changedByChecks"]))
    if res["missingTools"]:
        head.append("toolchain incomplete: %s" % ", ".join(res["missingTools"]))
    if res["definitionTouched"]:
        head.append("this branch changes how the checks are defined (%s): unverified until the CI runs"
                    % ", ".join(res["definitionTouched"]))
    head += ["  %-7s %-16s %6ss  ci: %s%s" % (s["result"], s["name"], s["seconds"], s["ci"],
                                               "  (%s)" % s["reason"] if s.get("reason") else "") for s in res["steps"]]
    foot = ["CI-only, the CI still runs these:"]
    foot += ["  %s: %s (%s)" % (c["check"], c["part"], c["why"]) for c in res["ciOnly"]]
    foot += ["full log: %s" % res["log"], "The CI stays the judge of a merge: a green result here is not the CI's."]
    failed = [s for s in res["steps"] if s["tail"]]
    room = SUMMARY_CAP - len("\n".join(head + foot)) - 1
    tails = []
    for i, s in enumerate(failed):
        title = "--- %s, last lines ---" % s["name"]
        keep = room // (len(failed) - i) - len(title) - 2
        if keep <= 0:
            continue
        body = "\n".join(s["tail"])
        if len(body) > keep:  # the tail's end matters most: cut from its start, at a line
            body = body[len(body) - keep:]
            body = body.split("\n", 1)[1] if "\n" in body else body
        tails.append(title + "\n" + body)
        room -= len(tails[-1]) + 1
    return "\n".join(head + tails + foot)[:SUMMARY_CAP]


def checks(decl: dict, base: str, blob: str, only: str | None, say, emit: bool = False) -> int:
    tools = pins(decl["tools"])
    missing = install(tools, say)
    steps = [s for s in decl["steps"] if only in (None, s["name"])]
    head = git("rev-parse", "HEAD").stdout.strip()
    before = changed()
    env = step_env(tools, missing)
    stem = head if only is None else "%s.step-%s" % (head, only)
    path = os.path.join(STATE, stem + ".log")
    start, budget = time.monotonic(), decl["minutes"] * MINUTE
    results, stopped = [], False
    with open(path, "a") as log:
        for s in steps:
            left = budget - (time.monotonic() - start)
            if stopped or left <= 0:
                results.append({"name": s["name"], "ci": s["ci"], "result": "skip", "seconds": 0, "exit": None, "tail": [],
                                "reason": "an earlier step stopped the run" if stopped else "the budget is spent"})
                continue
            r = run_step(s, env, min(s.get("timeoutMinutes", decl["minutes"]) * MINUTE, left), log, say)
            if r["result"] != "pass" and missing:
                r["reason"] = "toolchain incomplete"
            results.append(r)
            stopped = s.get("stopOnFailure", False) and r["result"] != "pass"
    res = {"version": 1, "head": head, "dirty": bool(before), "base": base, "declaration": blob, "tools": tools,
           "missingTools": missing, "steps": results, "partial": only is not None, "ciOnly": decl["ciOnly"],
           "changedByChecks": sorted(changed() - before), "definitionTouched": touched(base, decl["definedBy"]),
           "seconds": round(time.monotonic() - start, 1), "log": path}
    text = summary(res)
    record = {**res, "steps": [{k: v for k, v in r.items() if k != "tail"} for r in results]}
    if emit:
        # The harness's run (agent-run): the result goes to the pipe only agent-run holds, never to a
        # path the agent can write.
        print(json.dumps(record), flush=True)
        return 1 if any(r["result"] in ("fail", "timeout") for r in results) else 0
    with open(os.path.join(STATE, stem + ".json"), "w") as f:
        json.dump(record, f, indent=1)
    with open(os.path.join(STATE, stem + ".txt"), "w") as f:
        f.write(text + "\n")
    with open(os.path.join(STATE, LEDGER), "a") as f:
        f.write(json.dumps({"head": head, "partial": record["partial"], "seconds": record["seconds"],
                            "counts": dict(collections.Counter(r["result"] for r in results))}) + "\n")
    print(text, flush=True)
    return 1 if any(r["result"] in ("fail", "timeout") for r in results) else 0


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(prog="agent-checks", description="The CI's fast subset, in the sandbox.")
    ap.add_argument("--step", help="run this step only")
    ap.add_argument("--json", action="store_true", help="agent-run's run: say goes to stderr, the result to stdout, no files")
    ap.add_argument("--install", action="store_true", help="install the toolchain only")
    args = ap.parse_args(argv)

    def say(message):
        print("agent-checks: " + message, flush=True, file=sys.stderr if args.json else sys.stdout)
    try:
        base, blob, decl = declaration()
    except Undeclared as exc:
        say(str(exc))
        return 2
    if args.step and args.step not in [s["name"] for s in decl["steps"]]:
        say("no step %s; declared: %s" % (args.step, ", ".join(s["name"] for s in decl["steps"])))
        return 2
    os.makedirs(STATE, exist_ok=True)
    try:
        lock = open(os.path.join(STATE, ".lock"), "w")
    except OSError as exc:
        say("cannot take the lock: %s" % exc)
        return 1
    with lock:
        waited = time.monotonic()
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if args.json and time.monotonic() - waited > INSTALL_S + MINUTE:
                    say("the lock is still held after %ds" % (time.monotonic() - waited))
                    return 1
                say("waiting for another agent-checks (the toolchain install, or a run), %ds" % (time.monotonic() - waited))
                time.sleep(HEARTBEAT_S)
        if args.install:
            missing = install(pins(decl["tools"]), say)
            say("toolchain ready" if not missing else "toolchain incomplete: %s" % ", ".join(missing))
            return 1 if missing else 0
        try:
            return checks(decl, base, blob, args.step, say, args.json)
        except ToolchainError as exc:
            say(str(exc))
            return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
