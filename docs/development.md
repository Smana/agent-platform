# Development

**[CONTRIBUTING.md](../CONTRIBUTING.md) is the workflow** (prerequisites, `task check`, commits,
migrations, releases) and **[AGENTS.md](../AGENTS.md) is the coding standard** (conventions, the
package map, test rules, security rules). This page adds only what those two leave out: how the store
test harness mirrors the cluster, and the checks around a migration.

## The store test harness

Store tests start `postgres:18-alpine` with testcontainers, so they need a running Docker daemon.
The harness (`internal/store/testdb_test.go`) does what CNPG and Atlas do on the cluster:

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
  rather than in the Atlas operator. `task check` runs it as its `migrations` step:

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

A room page renders the log twice: the chat view (`web/src/chat.ts` — bubbles, tool rows
paired by `callId`, timeline markers) and the raw event stream, one toggle apart
(`web/src/roomview.ts`, remembered in `localStorage`). To read or restyle it without a
broker, `web/test/fixtures/live-session.jsonl` holds a recorded session and the preview
serves it statically:

```bash
npm --prefix web run preview   # http://127.0.0.1:4180/preview/preview.html (?live replays)
```

The palette is Maku's: `web/src/app.css` holds the design tokens (light first, a `.dark`
block) derived from the brand colours, Ink `#0A0A0F`, Surface `#14141C`, Paper `#F3F2FA`,
Core `#8B5CF6` and Muted `#9B9AAE`. Core marks and accents but is never normal text, so
`--primary`, `--link` and `--ring` are a derived violet that reaches 4.5:1;
`web/test/contrast.test.ts` pins every token and text pair, so a token change starts there.
`web/src/theme.ts` keeps the three-mode theme (light/dark/system, the `dark` class on
`<html>`, the `app-wizard:theme` storage key).

Room text is untrusted, so there are three rules, all tested in `web/test/render.test.ts`:

- **Parse once, as text.** markdown-it parses with HTML off, and each token becomes a DOM node
  whose text is set with `textContent`. Nothing builds HTML from a string: a test fails on
  `innerHTML` anywhere in `web/src/`.
- **Nothing is unescaped twice.** Markup the factory defused (`&lt;`, `!\[`, `]\:`) renders as the
  text it spells.
- **Only the web is linked.** A link must be http(s). An image shows its alt text and loads nothing.

The page loads nothing from another origin, under the CSP `humanapi` sets.

### Static assets

`web/src/assets/` is the one source of every static file, for the broker, the preview and the
demo alike:

| Path | What |
|---|---|
| `svg/maku-mark-{light,dark}.svg` | the header mark, byte-identical to `docs/assets/brand/svg/` |
| `favicon/` | `favicon.svg`, `favicon-16.png`, `favicon-32.png`, `apple-touch-icon.png`, byte-identical to `docs/assets/brand/favicon/` |
| `fonts/` | Space Grotesk latin 400, 500 and 700 as WOFF2, self-hosted under the SIL OFL 1.1 in `fonts/LICENSE` |

The fonts load with `font-display: swap` over a system-sans fallback, from this origin only: the
CSP allows `font-src 'self'` and nothing reaches a font CDN. `web/test/assets.test.ts` pins the
file list, the byte identity and every HTML entry's paths.

`npm run build` (through `task ui:build`) rewrites `internal/humanapi/ui/dist/` from scratch:

```text
dist/
├── index.html          src/index.html, served at / and /r/{id}
└── assets/             served at /assets/{file...}; a directory is a 404, never a listing
    ├── app.js          the esbuild bundle of src/main.ts
    ├── app.css         src/app.css
    └── svg/ favicon/ fonts/   copied from src/assets/
```

The preview server maps the same URLs onto the sources: `/assets/app.css` is `src/app.css`, and
any other `/assets/<file>` is `src/assets/<file>`. The demo (`web/scripts/demo-site.sh`) copies
`src/assets` to its `assets/` and rewrites `app.css`'s `/assets/` URLs to relative ones, so it
serves from any prefix.

## The UI preview

`npm --prefix web run preview` builds two pages and serves `web/` on localhost from recorded
fixtures, so it needs no broker, no cluster and no network. The first shows the log views
alone; the second is the whole room page: the title strip, what needs you, the notes, the
chat and raw views, the controls and the composer.

- `http://127.0.0.1:4180/preview/preview.html` (add `?live` to replay the session at reading pace)
- `http://127.0.0.1:4180/preview/full.html` (add `?theme=dark` for the dark palette)

The built files stay out of git. Buttons on the full page send nothing.

### Screenshots with Chromium

To review a visual change, capture the real preview headless with Chromium, both themes and two
widths, each with a fresh profile. Captures go to `${TMPDIR:-/tmp}/ui-screenshots/`, outside
git:

```bash
bash -c '
set -eu
npm --prefix web run preview:build
out=${TMPDIR:-/tmp}/ui-screenshots; mkdir -p "$out"
node web/preview/serve.mjs >"$out/server.log" 2>&1 & server_pid=$!
trap "kill $server_pid" EXIT
until curl -fs -o /dev/null http://127.0.0.1:4180/preview/full.html; do
  kill -0 "$server_pid" || { cat "$out/server.log"; exit 1; }; sleep 0.2
done
for theme in light dark; do
  for size in desktop:1440,1100 mobile:390,844; do
    chromium --headless --disable-gpu --hide-scrollbars --force-device-scale-factor=1 \
      --run-all-compositor-stages-before-draw --virtual-time-budget=3000 \
      --user-data-dir="$out/profile-${size%%:*}-$theme" --window-size="${size#*:}" \
      --screenshot="$out/${size%%:*}-$theme.png" "http://127.0.0.1:4180/preview/full.html?theme=$theme"
  done
done
'
```

The script runs in its own shell, so the server stops when the captures end, even pasted into an
interactive one; it waits for the server to answer before the first capture, and stops with the
server's log if the server exits first.

For the demo, build it with `bash web/scripts/demo-site.sh "${TMPDIR:-/tmp}/ui-demo"`, serve it
with `SERVE_ROOT="${TMPDIR:-/tmp}/ui-demo" PORT=4181 node web/preview/serve.mjs`, and capture
`http://127.0.0.1:4181/?theme=light` the same way with `--virtual-time-budget=180000`, so the
replay reaches its last frame.
