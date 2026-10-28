#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# The quickstart's shared checks and helpers, sourced by up.sh, db.sh, check.sh, e2e.sh and lib_test.sh.
# Functions only: sourcing it runs nothing.

# qs_platform_ok OS: Linux is the one platform the quickstart is proven on (install-anywhere spike,
# 2026-10-27). Elsewhere gVisor in kind and a port-forward to a loopback-only listener never ran.
qs_platform_ok() {
  [[ $1 == Linux ]] && return 0
  echo "the quickstart runs on Linux with Docker only: $1 is unproven (gVisor in kind and a port-forward to a loopback-only listener were never run there); see docs/quickstart.md#prerequisites" >&2
  return 1
}

# qs_node_minor IMAGE: the Kubernetes minor of a kind node image, kindest/node:v1.37.0@sha256:… → 37.
qs_node_minor() {
  local v=${1#*:v}
  v=${v%%@*}
  v=${v#*.}
  echo "${v%%.*}"
}

# qs_skew_ok CLIENT SERVER: kubectl supports one minor version of skew either way. Minors may carry
# a suffix ("37+").
qs_skew_ok() {
  local c=${1//[!0-9]/} s=${2//[!0-9]/}
  if [[ -n $c && -n $s ]] && ((c - s <= 1 && s - c <= 1)); then return 0; fi
  echo "kubectl 1.${c:-?} cannot drive a 1.$s cluster: kubectl supports one minor version of skew (1.$((s - 1)) to 1.$((s + 1))); install a matching kubectl (mise install kubectl)" >&2
  return 1
}

# qs_unsandboxed VALUE: ALLOW_UNSANDBOXED is unset (0) or exactly 1 (1); anything else stops, so a
# typo never silently drops or keeps the sandbox.
qs_unsandboxed() {
  case $1 in
  "") echo 0 ;;
  1) echo 1 ;;
  *) echo "ALLOW_UNSANDBOXED is exactly 1 or unset, not '$1'" >&2; return 1 ;;
  esac
}

# qs_port_ok PORT: the local port the broker is forwarded to, and its publicURL's port.
qs_port_ok() {
  [[ $1 =~ ^[1-9][0-9]*$ ]] && (($1 >= 1024 && $1 <= 65535)) && return 0
  echo "LOCAL_PORT '$1' is not a port in 1024-65535" >&2
  return 1
}

# qs_retry_webhook TRIES DELAY CMD...: runs CMD, retrying only while an admission webhook does not
# answer yet. An operator's Deployment reports ready a moment before its webhook serves (CNPG,
# 2026-10-28: "failed calling webhook mcluster.cnpg.io"). Any other error returns at once.
qs_retry_webhook() {
  local tries=$1 delay=$2 i err
  shift 2
  err=$(mktemp)
  for ((i = 1; i <= tries; i++)); do
    if "$@" 2>"$err"; then rm -f "$err"; return 0; fi
    if ! grep -q 'failed calling webhook' "$err" || ((i == tries)); then
      cat "$err" >&2; rm -f "$err"; return 1
    fi
    sleep "$delay"
  done
}

# qs_secret NS NAME TYPE KEY=VAR...: a Secret manifest on stdout. Each value is read by yq from the
# environment variable VAR (strenv), so no value is ever a command-line argument (/proc/<pid>/cmdline
# is world-readable). Pipe it to kubectl apply -f -.
qs_secret() {
  local ns=$1 name=$2 type=$3 pair key var
  shift 3
  local expr='.apiVersion = "v1" | .kind = "Secret" | .metadata.name = strenv(QS_NAME) | .metadata.namespace = strenv(QS_NS) | .type = strenv(QS_TYPE)'
  for pair in "$@"; do
    key=${pair%%=*} var=${pair#*=}
    [[ $key =~ ^[A-Za-z0-9._-]+$ && $var =~ ^[A-Z_][A-Z0-9_]*$ ]] || { echo "qs_secret: '$pair' is not key=ENV_VAR" >&2; return 1; }
    [[ -n ${!var+set} ]] || { echo "qs_secret: $var is unset" >&2; return 1; }
    export "${var?}"
    expr+=" | .stringData.\"$key\" = strenv($var)"
  done
  QS_NS=$ns QS_NAME=$name QS_TYPE=$type yq -n "$expr"
}

# qs_fetch_roomctl VERSION DEST: the release's maku for this Linux machine, checked against the
# release's maku.sha256, installed 0755 at DEST.
qs_fetch_roomctl() {
  local arch tmp base rc=0
  case $(uname -m) in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "no maku release for $(uname -m)" >&2; return 1 ;;
  esac
  base="https://github.com/Smana/maku/releases/download/$1"
  tmp=$(mktemp -d)
  if ! {
    curl -fsSL -o "$tmp/maku-linux-$arch" "$base/maku-linux-$arch" &&
      curl -fsSL -o "$tmp/maku.sha256" "$base/maku.sha256" &&
      (cd "$tmp" && grep " maku-linux-$arch\$" maku.sha256 | sha256sum -c - >/dev/null) &&
      install -m 0755 "$tmp/maku-linux-$arch" "$2"
  }; then
    rc=1
  fi
  rm -rf "$tmp" # this function's own temp dir
  ((rc == 0)) || echo "could not fetch maku $1 for linux-$arch (or its checksum did not match)" >&2
  return "$rc"
}
