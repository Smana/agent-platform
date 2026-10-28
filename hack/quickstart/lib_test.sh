#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit tests of lib.sh's pure functions; run by check.sh (task quickstart:check). No cluster, no docker.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/quickstart/versions.env
. hack/quickstart/versions.env
# shellcheck source=hack/quickstart/lib.sh
. hack/quickstart/lib.sh
fail=0
ok() { "$@" 2>/dev/null || { echo "FAIL: expected success: $*" >&2; fail=1; }; }
no() { if "$@" 2>/dev/null; then echo "FAIL: expected failure: $*" >&2; fail=1; fi; }
eq() { [[ $1 == "$2" ]] || { echo "FAIL: $3: got '$1', want '$2'" >&2; fail=1; }; }

ok qs_platform_ok Linux
no qs_platform_ok Darwin
no qs_platform_ok MINGW64_NT-10.0
[[ $(qs_platform_ok Darwin 2>&1) == *"Linux with Docker only"* ]] || { echo "FAIL: the platform refusal names the reason" >&2; fail=1; }

eq "$(qs_node_minor "$KIND_NODE_IMAGE")" 37 "node minor of KIND_NODE_IMAGE"
eq "$(qs_node_minor kindest/node:v1.36.2)" 36 "node minor without a digest"
ok qs_skew_ok 37 37; ok qs_skew_ok 36 37; ok qs_skew_ok 38 37; ok qs_skew_ok "37+" 37
no qs_skew_ok 35 37; no qs_skew_ok 29 37; no qs_skew_ok "" 37
[[ $(qs_skew_ok 29 37 2>&1) == *"kubectl 1.29 cannot drive a 1.37 cluster"* ]] || { echo "FAIL: the skew refusal names both versions" >&2; fail=1; }

eq "$(qs_unsandboxed "")" 0 "unset ALLOW_UNSANDBOXED"
eq "$(qs_unsandboxed 1)" 1 "ALLOW_UNSANDBOXED=1"
no qs_unsandboxed true; no qs_unsandboxed 0; no qs_unsandboxed yes

ok qs_port_ok 8080; ok qs_port_ok 18080; no qs_port_ok 80; no qs_port_ok 70000; no qs_port_ok 8o80; no qs_port_ok 010000; no qs_port_ok 08080

# qs_secret: the value comes from the environment, survives quoting, and is never an argument
export QS_TEST_KEY=$'s3cr"et $HOME `id`\nline2'
m=$(qs_secret agents agent-model Opaque api_key=QS_TEST_KEY)
eq "$(yq '.stringData.api_key' <<<"$m")" "$QS_TEST_KEY" "secret value round-trips"
eq "$(yq '.metadata.namespace + "/" + .metadata.name + " " + .type' <<<"$m")" "agents/agent-model Opaque" "secret metadata"
no qs_secret agents x Opaque 'api key=QS_TEST_KEY'
no qs_secret agents x Opaque api_key=lowercase_var
unset QS_UNSET_VAR || true
no qs_secret agents x Opaque api_key=QS_UNSET_VAR
# the function body never expands a value: every value reaches yq through strenv()
body=$(declare -f qs_secret)
# shellcheck disable=SC2016 # the literal ${!var} text is what must be absent
[[ $body != *'${!var}'* ]] || { echo "FAIL: qs_secret expands a variable's value" >&2; fail=1; }

# no script passes a literal secret on a command line
if grep -n -- '--from-literal' hack/quickstart/*.sh | grep -v '^hack/quickstart/lib_test.sh:'; then
  echo "FAIL: --from-literal puts a value on argv: use qs_secret" >&2; fail=1
fi
[[ $fail == 0 ]] && echo "lib_test: ok"
exit "$fail"
