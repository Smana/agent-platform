# Development

**[CONTRIBUTING.md](../CONTRIBUTING.md) is the workflow** (prerequisites, `task check`, commits,
migrations, releases) and **[AGENTS.md](../AGENTS.md) is the coding standard** (conventions, the
package map, test rules, security rules). This page adds only what those two leave out: how the store
test harness mirrors the cluster, and the checks around a migration.

## The store test harness

Store tests start `postgres:18-alpine` with testcontainers, so they need a running Docker daemon.
The harness (`internal/store/testdb_test.go`, AP-1) does what CNPG and Atlas do on the cluster:

| Step | Mirrors |
|---|---|
| Create the login roles `rooms_owner`, `rooms_broker` and `rooms_retention` | CNPG's managed roles from the `SQLInstance` claim |
| Make `rooms_owner` own the `rooms` database | The claim's `databases[].owner` |
| Apply every file in `internal/store/migrations/`, in name order, as `rooms_owner` | The Atlas operator |
| Hand back one connection string per role | The Secrets `xplane-rooms-cnpg-role-*` (key `uri`) |

A grant or policy that works only for a superuser therefore fails here, not in the cluster. A test
that proves a database guarantee connects **as the role it constrains** and asserts the exact
SQLSTATE, for example `42501` (insufficient privilege) when `rooms_broker` tries
`UPDATE events`. The guarantees and their tests are listed in
[room log](room-log.md#the-guarantees).

```bash
go test -race ./internal/store/...
go test -race -run TestBrokerRoleIsAppendOnly -v ./internal/store/
```

## Around a migration

CONTRIBUTING.md has the steps to add one. Two checks it does not spell out:

- **Validate the directory** after re-hashing, so a stale or reordered `atlas.sum` fails locally
  rather than in the Atlas operator:

  ```bash
  atlas migrate validate --dir file://internal/store/migrations
  ```

- **Write for `rooms_owner`, which has no `CREATEROLE`.** A migration grants to roles CNPG creates;
  it never creates them. In the cluster, the migration fails until CNPG has created them, and the
  Atlas operator retries.

Why `atlas.sum` exists, which migration each phase adds, and how `atlasSchema.ref` tracks them:
[room log](room-log.md#migrations-with-atlas).

## The web UI

`web/` is TypeScript, bundled by esbuild into `internal/humanapi/ui/dist/`. The bundle is
committed and embedded with `go:embed`, so the broker image builds without Node, and
`task ui:check` fails when the committed bundle differs from what `web/` builds. After a change
under `web/`:

```bash
task ui:test     # tsc and vitest
task ui:build    # rewrite dist/; commit it with the source
```

Room text is untrusted, so there are three rules, all tested in `web/test/render.test.ts`:

- **Parse once, as text.** markdown-it parses with HTML off, and each token becomes a DOM node
  whose text is set with `textContent`. Nothing builds HTML from a string: a test fails on
  `innerHTML` anywhere in `web/src/`.
- **Nothing is unescaped twice.** Markup the factory defused (`&lt;`, `!\[`, `]\:`) renders as the
  text it spells.
- **Only the web is linked.** A link must be http(s). An image shows its alt text and loads nothing.

The page loads nothing from another origin, under the CSP `humanapi` sets.
