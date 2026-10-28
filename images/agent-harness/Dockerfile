# syntax=docker/dockerfile:1
# The AgentRun harness (SP1 design §5, ADR-0041): OpenHands agent-server plus
# the driver, the git credential helper, gh with the PR provenance footer, and
# the Agent-Run trailer hook.
# The AgentRun composition pins this image by digest; a claim never names one.
FROM ghcr.io/openhands/agent-server:1.56.0-python@sha256:718bf6d719f34184810ecaebe607f1aafaf526e109654815e61ddd6238728023 AS harness

# cli/cli release: Dependabot cannot bump an ARG; move GH_VERSION and both sums together by hand.
ARG GH_VERSION=2.101.0
ARG GH_SHA256_AMD64=9bca2d1c16825f109907a23307628a2f0698fbf99662b73a5cf0b020293072b8
ARG GH_SHA256_ARM64=b57e8063f18862647c9d22727c32e9da1b963f8bf9db648fe123a6975695640f

# jdx/mise release (v3 feature 3): agent-checks installs a repository's declared tools with it, at
# the versions its mise.toml pins. Move MISE_VERSION and both sums together by hand.
ARG MISE_VERSION=2026.10.3
ARG MISE_SHA256_AMD64=04147c68e946902f5226dfdcd54d19907aed3cf54d95b2f27d2b9c778bb26f9e
ARG MISE_SHA256_ARM64=e79866e32624b346f6854d93ca8a24294516cd7c0b48ce0e80af508fb7c3d8b8
ARG TARGETARCH

USER root
# gh, checksum-pinned per architecture. The upstream image gives its user
# passwordless sudo; nothing here needs it and NoNewPrivileges is not reliable
# under gVisor (design §1), so sudo and every setuid and setgid bit go.
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) sum="${GH_SHA256_AMD64}" msum="${MISE_SHA256_AMD64}" march=x64 ;; \
      arm64) sum="${GH_SHA256_ARM64}" msum="${MISE_SHA256_ARM64}" march=arm64 ;; \
      *) exit 1 ;; \
    esac; \
    fetch() { /usr/local/bin/python -c "import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])" "$1" "$2"; }; \
    fetch "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz" /tmp/gh.tgz; \
    echo "${sum}  /tmp/gh.tgz" | sha256sum -c -; \
    tar -xzf /tmp/gh.tgz -C /tmp; \
    install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh" /usr/local/lib/gh-real; \
    rm -rf /tmp/gh.tgz "/tmp/gh_${GH_VERSION}_linux_${TARGETARCH}"; \
    fetch "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-${march}.tar.gz" /tmp/mise.tgz; \
    echo "${msum}  /tmp/mise.tgz" | sha256sum -c -; \
    tar -xzf /tmp/mise.tgz -C /tmp; \
    install -D -m 0755 /tmp/mise/bin/mise /opt/agent/bin/mise; \
    rm -rf /tmp/mise.tgz /tmp/mise; \
    apt-get update; \
    apt-get install -y --no-install-recommends rsync ripgrep; \
    rm -rf /var/lib/apt/lists/*; \
    rm -f /etc/sudoers.d/*; \
    if [ -f /etc/sudoers ]; then sed -i '/NOPASSWD/d' /etc/sudoers; fi; \
    find / -xdev -type f -perm /6000 -exec chmod ug-s {} +

# The SDK agent-run imports; see requirements.in for why it is not in the base image.
# --only-binary refuses any sdist build, which QEMU makes slow or breaks for
# arm64 (SP1 design). func-timeout (via openhands-tools) ships no wheel for
# any platform on PyPI, but is pure Python, so its sdist build is a plain
# setuptools invocation with no compiler involved either way; verified with
# `pip download --only-binary=:all: -r requirements.txt` for both
# manylinux_x86_64 and manylinux_aarch64 that every other package has a wheel.
COPY requirements.txt /tmp/requirements.txt
RUN /usr/local/bin/python -m venv /agent-server/.venv \
 && /agent-server/.venv/bin/pip install --no-cache-dir --require-hashes \
      --only-binary=:all: --no-binary=func-timeout -r /tmp/requirements.txt \
 && rm /tmp/requirements.txt

# The system python3 the repositories' checks run gets the venv's PyYAML (v3 feature 3, R4).
COPY checks-requirements.txt /tmp/checks-requirements.txt
RUN /usr/local/bin/python -m pip install --no-cache-dir --root-user-action=ignore --require-hashes --only-binary=:all: \
      -r /tmp/checks-requirements.txt \
 && rm /tmp/checks-requirements.txt

COPY --chmod=0755 agent_run.py agent_checks.py test_contract.py git_credential_agent.py loop_policy.py pr_footer.py /opt/agent/
COPY --chmod=0755 site /opt/agent/site
COPY --chmod=0755 gh /usr/local/bin/gh
COPY --chmod=0755 commit-msg /etc/agent/git-hooks/commit-msg
COPY --chmod=0755 pre-commit /etc/agent/git-hooks/pre-commit
COPY gitconfig /etc/gitconfig
RUN ln -s /opt/agent/agent_run.py /usr/local/bin/agent-run \
 && ln -s /opt/agent/agent_checks.py /usr/local/bin/agent-checks \
 && ln -s /opt/agent/test_contract.py /usr/local/bin/test-contract \
 && ln -s /opt/agent/git_credential_agent.py /usr/local/bin/git-credential-agent

# agent-server otherwise starts openvscode-server on 0.0.0.0:8001, putting a
# terminal on the pod network that nothing in a headless run uses.
ENV OH_ENABLE_VSCODE=false
# git must never fall back to an interactive prompt in a sandbox with no tty.
ENV GIT_TERMINAL_PROMPT=0
USER 10001
ENTRYPOINT ["tini", "--", "/usr/local/bin/agent-run"]

# `task harness:test` runs every suite against the SDK in the image.
FROM harness AS test
COPY --chown=10001:10001 tests /opt/agent/tests
# The lock test compares the two requirement files, which the published image does not carry.
COPY --chown=10001:10001 requirements.txt checks-requirements.txt /opt/agent/
# search's grep and glob run only on ripgrep: a base image or a package change that drops it fails here.
RUN rg --version
USER root
# pytest for test-contract's pytest fixture (v3 feature 4): the test stage only, never the shipped image.
RUN /usr/local/bin/python -m pip install --no-cache-dir --root-user-action=ignore --require-hashes --only-binary=:all: \
      -r /opt/agent/tests/contract-requirements.txt
USER 10001
# A missing pytest is a failure here, never a skip: the fixture must run in the image.
ENV TEST_CONTRACT_REQUIRE_PYTEST=1
RUN python3 -c "import pytest" \
 && cd /opt/agent && /agent-server/.venv/bin/python -m unittest discover -s tests -v

# The default target, and what CI publishes.
FROM harness
