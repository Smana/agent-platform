# Contributing

The coding conventions, the package map and the security rules are in [AGENTS.md](AGENTS.md),
the one guide for humans and agents alike. This page covers the workflow around them.

## Prerequisites

| Tool | Why |
|---|---|
| [mise](https://mise.jdx.dev) | Installs the pinned Go, Node.js (the web UI's build), golangci-lint, task and govulncheck from `mise.toml` |
| Docker | Store tests start PostgreSQL with testcontainers |
| [Atlas](https://atlasgo.io) | Hashes the migration directory; pinned in `mise.toml` |

```bash
mise trust && mise install
task check
```

`mise trust` is needed once per checkout: mise ignores an untrusted `mise.toml`, and the lint
wrapper then falls back to whatever golangci-lint is on PATH (it refuses a v1).

## The gate

`task check` must exit 0 before every commit and push. It runs eleven steps (SPDX headers, tidy,
lint, vuln, test, CRD check, chart check, migrations, docs lint, UI test, UI check); CI's required `check`
job runs the same command. What each step catches is in the [table in AGENTS.md](AGENTS.md#quality-gate-run-before-every-commit).

Every new `.go` file starts with:

```go
// SPDX-License-Identifier: Apache-2.0
```

## Commits and pull requests

- **Conventional commits**, in English: `feat(store): …`, `fix(bridge): …`, `test(…)`,
  `docs: …`, `ci: …`, `chore: …`. Add `!` after the scope for a breaking change.
- **One concern per commit.** A refactor and the feature it enables are two commits.
- **Branch from the latest `origin/main`** and rebase onto it before pushing; never merge
  `main` into a branch.
- **Squash-merge only.** It is the only merge method enabled, so the **PR title becomes the
  commit on `main`**: it must itself be a conventional commit.
- The repository ruleset `main` keeps history linear: changes land through a pull request, no
  force push, no deletion, and the `check`, `analyze` (CodeQL) and `harness-test` checks must
  pass. A repository admin may bypass it, through a pull request only.
- Say in the PR what changed and how you verified it, citing the gate's exit code.

## Adding a database migration

Migrations live in `internal/store/migrations/` and are applied in the cluster by the Atlas
operator, as the database owner, from the `atlas-db-migrations` ConfigMap.

1. Add `internal/store/migrations/<UTC yyyymmddhhmmss>_<what>.sql`. Never edit a migration that
   has shipped in a release: add a new one.
2. List it in `internal/store/migrations/kustomization.yaml`.
3. Re-hash the directory and commit `atlas.sum` with the SQL:

   ```bash
   atlas migrate hash --dir file://internal/store/migrations
   ```

4. Keep the broker append-only: no migration widens `rooms_broker`'s grants or disables row-level
   security (see the security rules in AGENTS.md).
5. `task test` applies the whole directory to a real PostgreSQL and runs the store tests.

## Releases

| Trigger | What ships |
|---|---|
| A push to a PR branch of this repo | `ghcr.io/smana/{room-broker,room-bridge,agent-factory,agent-harness}:v<next-patch>-pr<N>.<sha8>`, for integration testing (`ci.yaml`) |
| | The chart `oci://ghcr.io/smana/charts/agent-factory`, at `<next-patch>-pr<N>.g<sha8>` (`ci.yaml`, `chart-prerelease`) |
| A `v*` tag on `main` | The four images `ghcr.io/smana/{room-broker,room-bridge,agent-factory,agent-harness}:vX.Y.Z` |
| | The chart `oci://ghcr.io/smana/charts/agent-factory`, at the tag's version, signed |
| | On the GitHub release: `roomctl-{os}-{arch}` (linux and darwin, amd64 and arm64), `roomctl.sha256` and `crd-rooms.yaml` (the `Room` CRD) |

To release, tag a commit on `main` that passed CI and push the tag:

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

`release.yaml` re-runs `task check` (a tag can point at an untested commit), then builds each
image for `linux/amd64` and `linux/arm64` with SLSA provenance and an SBOM, and signs each
digest with keyless cosign. Nothing is tagged `latest`: cloud-native-ref pins each image by
digest, so a release reaches a cluster only through a pin bump there. The verification command
is in the README.

Pre-1.0, a breaking change bumps the minor version.

## Reporting a vulnerability

Privately, never in a public issue: see [SECURITY.md](SECURITY.md).
