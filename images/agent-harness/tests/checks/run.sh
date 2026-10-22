#!/usr/bin/env bash
# v3 feature 3, offline proof 4: the harness image under gVisor (runsc), on a network whose only way
# out is a proxy allowing exactly a check-enabled run's hosts, clones REPO's main and runs its
# declared checks. Fails on a refused host (named), on a step that ends fail or timeout, and on an
# install that leaves a tool missing. Records the install's and each step's seconds and mise's
# GitHub API calls in $GITHUB_STEP_SUMMARY when set.
#
# Usage: run.sh IMAGE OWNER/REPO PROFILES TOKEN_FILE   e.g. run.sh agent-harness:checks Smana/ai-native-ref golang,pypi /tmp/token
# PROFILES are the repository's checks.egress (the factory config); github is always on.
set -euo pipefail
image=$1 repo=$2 profiles=$3 token=$4
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The composition's _EGRESS_PROFILES, the two v3 hosts included; runctl's defaultProfiles are the same.
declare -A HOSTS=(
  [github]="github.com api.github.com codeload.github.com objects.githubusercontent.com raw.githubusercontent.com release-assets.githubusercontent.com"
  [pypi]="pypi.org files.pythonhosted.org"
  [npm]="registry.npmjs.org"
  [golang]="proxy.golang.org sum.golang.org dl.google.com"
  [crates]="index.crates.io static.crates.io"
)

if ! curl -fsS -o /dev/null "https://raw.githubusercontent.com/${repo}/HEAD/.github/agent-checks.yaml"; then
  echo "SKIP ${repo}: its main declares no checks yet"
  exit 0
fi

allow=${HOSTS[github]}
for p in ${profiles//,/ }; do
  [ -n "${HOSTS[$p]:-}" ] || { echo "unknown profile $p" >&2; exit 2; }
  allow="$allow ${HOSTS[$p]}"
done

name="checks-$(echo "$repo" | tr '/A-Z' '-a-z')"
out="$(mktemp -d)"
cleanup() {
  docker rm -f "$name-proxy" >/dev/null 2>&1 || true
  docker network rm "$name" >/dev/null 2>&1 || true
  docker volume rm "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup
docker network create --internal "$name" >/dev/null
docker volume create "$name" >/dev/null
chmod 0777 "$out"

# The proxy: the image's own Python, on the default bridge (the way out) and on the internal network.
docker run -d --name "$name-proxy" --network bridge -e ALLOW="${allow// /,}" -e LOG=/out/proxy.log \
  -v "$here/proxy.py:/proxy.py:ro" -v "$out:/out" --entrypoint /usr/local/bin/python "$image" /proxy.py 3128 >/dev/null
docker network connect "$name" "$name-proxy"

# /workspace as the composition gives it: writable by uid 10001, the root filesystem read-only.
docker run --rm --user 0 -v "$name:/workspace" --entrypoint chown "$image" 10001:10001 /workspace

docker run --rm --runtime=runsc --network "$name" --read-only --cpus 2 --memory 4g \
  --tmpfs /tmp:size=2g,mode=1777 --tmpfs /home/openhands:size=2g,uid=10001,gid=10001 \
  -v "$name:/workspace" -v "$out:/out" -v "$token:/run/agent/github/token:ro" \
  -e HOME=/home/openhands -e HTTPS_PROXY="http://$name-proxy:3128" -e GIT_TOKEN_FILE=/run/agent/github/token \
  -e MISE_LOG_LEVEL=debug -e REPO="$repo" \
  --entrypoint bash "$image" -c '
    set -u
    dmesg 2>/dev/null | head -1 > /out/kernel
    git clone --quiet "https://github.com/$REPO.git" /workspace/repo || exit 3
    s=$SECONDS; agent-checks --install > /out/install.out 2>&1; echo $? > /out/install.exit; echo $((SECONDS - s)) > /out/install.seconds
    agent-checks > /out/checks.out 2>&1; echo $? > /out/checks.exit
    cp /workspace/.agent-checks/*.json /workspace/.agent-checks/install.log /out/ 2>/dev/null || true'

fail=0
grep -qi gvisor "$out/kernel" || { echo "FAIL ${repo}: the container did not run under gVisor ($(cat "$out/kernel"))"; fail=1; }
if grep '^refuse' "$out/proxy.log" | sort -u | grep .; then
  echo "FAIL ${repo}: the checks reached hosts beyond the run's egress (above)"; fail=1
fi
[ "$(cat "$out/install.exit")" = 0 ] || { echo "FAIL ${repo}: the install left a tool missing"; cat "$out/install.out"; fail=1; }
cat "$out/checks.out"
python3 - "$out" "$repo" <<'EOF' || fail=1
import glob, json, os, re, sys
out, repo = sys.argv[1:3]
res = [json.load(open(p)) for p in glob.glob(os.path.join(out, "*.json")) if ".step-" not in p]
assert len(res) == 1, "want one result, got %d" % len(res)
steps = res[0]["steps"]
bad = [s["name"] for s in steps if s["result"] not in ("pass", "skip")]
api = len(re.findall(r"GET https://api\.github\.com\S*$", open(os.path.join(out, "install.log")).read(), re.M))
rows = ["### %s: install %ss, %d GitHub API calls" % (repo, open(os.path.join(out, "install.seconds")).read().strip(), api),
        "", "| step | result | seconds |", "|---|---|---|"] + ["| %s | %s | %s |" % (s["name"], s["result"], s["seconds"]) for s in steps]
if os.environ.get("GITHUB_STEP_SUMMARY"):
    open(os.environ["GITHUB_STEP_SUMMARY"], "a").write("\n".join(rows) + "\n\n")
print("\n".join(rows))
if bad:
    sys.exit("FAIL %s: %s did not pass" % (repo, ", ".join(bad)))
EOF
exit "$fail"
