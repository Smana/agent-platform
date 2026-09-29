# Contributing

The coding conventions, the package map and the security rules are in [AGENTS.md](AGENTS.md),
the one guide for humans and agents alike. This page covers the workflow around them.

## Prerequisites

| Tool | Why |
|---|---|
| [mise](https://mise.jdx.dev) | Installs the pinned Go, golangci-lint, task and govulncheck from `mise.toml` |
| Docker | Store tests start PostgreSQL with testcontainers (from SP2 phase 1) |
| [Atlas](https://atlasgo.io) | Hashes the migration directory; pinned in `mise.toml` once the first migration lands |

```bash
mise trust && mise install
task check
```

`mise trust` is needed once per checkout: mise ignores an untrusted `mise.toml`, and the lint
wrapper then falls back to whatever golangci-lint is on PATH (it refuses a v1).

## The gate

`task check` must exit 0 before every commit and push. It runs the SPDX header check,
`go mod tidy -diff`, golangci-lint, govulncheck and `go test -race`; CI's required `check` job
runs the same command. The table of what each step catches is in AGENTS.md.

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
- The `main` ruleset keeps history linear: no force push, no deletion, and the `check` and
  `analyze` (CodeQL) checks must pass.
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
| A push to a PR branch of this repo | `ghcr.io/smana/room-{broker,bridge}:v<next-patch>-pr<N>.<sha8>`, for integration testing |
| A `v*` tag on `main` | `ghcr.io/smana/room-{broker,bridge}:vX.Y.Z` |

To release, tag a commit on `main` that passed CI and push the tag:

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

`release.yaml` re-runs `task check` (a tag can point at an untested commit), then builds both
images for `linux/amd64` and `linux/arm64` with SLSA provenance and an SBOM, and signs each
digest with keyless cosign. Nothing is tagged `latest`: cloud-native-ref pins each image by
digest, so a release reaches a cluster only through a pin bump there. The verification command
is in the README.

Pre-1.0, a breaking change bumps the minor version.

## Reporting a vulnerability

Privately, never in a public issue: see [SECURITY.md](SECURITY.md).
