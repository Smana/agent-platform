# Adoption, part 1: the story and the live demo: design

**Status:** design, approved by the owner on 2026-10-18 in brainstorming; awaiting review of this
written spec.

**Scope:** `README.md`, `docs/concepts.md`, `docs/architecture.md`, a published live-replay demo
built from `web/preview/`, and the CI job that publishes it. No change to a binary's behaviour.

**Programme:** this is part 1 of three, each with its own spec, plan and build cycle:

- **1. The story and the demo:** this spec.
- **2. The name.** It is chosen with this pitch in hand. The README keeps "agent-platform" until
  then, and the rewrite puts the name in one place so the rename is one edit.
- **3. Install anywhere:** an `AgentRun` that does not need cloud-native-ref, an umbrella chart
  and a kind quickstart. It is the real adoption gate and the larger piece.

## Conclusion

A platform team that lands on the repository today reads the mechanics before it reads the
point. Line 3 reaches "an append-only, redacted Postgres log", "four binaries" and "pinned by
digest". A room is defined by how it is built ("a Postgres log"), not by what it gives the reader.
The specs' codes (SP2, SP3) appear on the first screen. The README says it was "built for one
maintainer's platform", and nothing on the page can be seen or tried.

The fix:

- **The README becomes a landing page.** It leads with one promise, *oversight you can trust*,
  backs it with three proof points, and shows the product: a screenshot that opens a
  **live replay** of a recorded run.
- **A room is explained by analogy** first, then by mechanics.
- **The internals move down** to the architecture page.

## Decisions

| # | Decision | Rejected | Why |
|---|---|---|---|
| A1 | **The audience is platform teams running Kubernetes**: the people who would install it, and who today refuse coding agents on their cluster | Readers of cloud-native-ref; both, staged | The owner's choice. It decides the tone (governance, not productivity) and makes part 3 necessary |
| A2 | **The headline promise is oversight you can trust**: *Run coding agents on your Kubernetes cluster without trusting them.* | "Issue in, PR out" (productivity); "work with agents live" (collaboration) | The buyer who says no today is the platform or security team. The factory and the room's collaboration become proof points, not the headline |
| A3 | **The README is the landing page**, in the repository, still checked by `hack/docs.sh` | A separate docs site; a rewrite of the first screen only | One place to maintain, links kept honest by the existing check. A first-screen-only rewrite leaves "what is a room" on a page that starts from Postgres |
| A4 | **The proof is a live replay**: the room page, playing a recorded run at recorded pace, sped up, published as a static page | Screenshots and a GIF; screenshots alone | A visitor opens a step, sees an approval wait and a rejection land, and tries the composer, with no cluster. The page already renders offline from fixtures (`npm run preview`, D10 of the readability spec) |
| A5 | **A room is introduced by analogy before mechanics**: a flight recorder and a group chat for one agent's run | Leading with the log's properties | The analogy carries both halves: live (watch, steer, approve) and permanent (audit, fork, hand off). The properties follow as the reasons it can be trusted |
| A6 | **Internals move to `docs/architecture.md`**: the binaries table, digest pinning, release mechanics, SP codes | Deleting them | Operators and contributors still need them, on the second click |
| A7 | **The status is stated honestly, once**: *runs today on cloud-native-ref; a portable install is the next milestone*, linking part 3 | Hiding the dependency | A platform team will find it within a minute. Saying it first earns the trust the headline asks for. It also respects `hack/docs.sh`'s one-status rule |

## The README

### The first screen

It must fit without scrolling at 1280×800. Copy, verbatim except for the name:

```markdown
# agent-platform

**Run coding agents on your Kubernetes cluster without trusting them.**

- **Sandboxed:** every run is an isolated pod holding only short-lived, scoped credentials, inside a budget it cannot raise.
- **Supervised:** approvals, steering and the merge gate sit outside the sandbox, with humans.
- **On the record:** every run streams into a room that nobody can rewrite, the agent included.

[![A room replaying an agent run: steps, an approval waiting, the composer](docs/assets/room-dark.png)](<demo URL>)

**[Watch a run in the live demo →](<demo URL>)** · [How it works](#how-it-works) · [What's a room?](#whats-a-room)
```

The screenshot is the dark theme at 1280 px, made by the preview screenshot script and committed
under `docs/assets/`. A light variant is served through `<picture>` with `prefers-color-scheme`.

### Then, in this order

1. **What's a room?** One paragraph, analogy first:
   *"A room is a flight recorder and a group chat for one piece of agent work. While the run is
   live, you watch every step, steer the agent, and approve what it may not do alone. Afterwards
   the room stays: an ordered, redacted record that nobody can edit, which you audit, fork from
   any message, or hand to the next run."*
   Then three short lines of why it can be trusted: append-only storage the broker cannot rewrite,
   secrets redacted before they are stored, every event stamped with an authenticated identity.
   Each line links to `docs/room-log.md` or `docs/security.md`.
2. **How it works.** A six-box mermaid flowchart, *labelled issue → triage → sandboxed run →
   room → pull request → human merge*, with one sentence per box under it.
3. **Who it's for, and who it isn't.**
   - For: platform teams who want agents on their cluster under their own rules.
   - Not yet for: a laptop tool, or a hosted service.
4. **Status (A7).** One paragraph: the version, daily use on one cloud, the merge gate in shadow,
   and that it runs today on cloud-native-ref, with a portable install next. Links to the
   platform's status page.
5. **Start here.** The existing reader table (new here, maintainer, CLI, operator, contributor),
   unchanged.
6. **The rest:** Develop and License, unchanged. The Architecture, Binaries and images, Releases
   and Where it plugs in sections move to `docs/architecture.md` (A6), with one link to it.

**Every claim on the first screen holds against [docs/security.md](../../security.md):** short-lived credentials are T4's 600 s run token and octo-sts's minted GitHub tokens; "nobody can rewrite" is T12 and the room log's grants and triggers. A claim the code stops backing is a docs bug.

**Banned on the first screen:** SP codes, binary names, Postgres, digests, CRD groups. They are
defined where they first appear further down.

## `docs/concepts.md`

- **Lead with the room analogy** (the README's paragraph, expanded by one example) before the
  glossary.
- **Add a short "In one picture" diagram** before the tables: one room, its participants (humans,
  a run, the factory), and what flows in and out.
- **Define the glossary's jargon on first use** (C2 id, principal, driver epoch), or link it. Step
  and call line join the glossary, matching the room page.
- **The tables stay**: they are the reference half of the page.

## `docs/architecture.md`

It gains the sections the README sheds (A6): binaries and images, releases, where it plugs in. The
content is unchanged. Anchors are re-pointed, and `hack/docs.sh` proves no link broke.

## The live replay demo

### What a visitor sees

The room page as #148 leaves it (this part builds on that PR, which merges first), playing a recorded run:

- **Playback:** events land at the recorded pace, sped up 8× by default, so steps open while a
  call runs and close when it ends, an approval card appears in Needs you, and a rejection turns
  a step amber.
- **Controls:** a small bar above the dock: play/pause, speed (1×, 8×, 32×) and *skip to the end*.
- **Acts:** acts are inert. Every act (send, approve, deny, fork, give) shows *"Demo: nothing is
  sent."* in the dock's notice. The composer and the approval buttons still work as controls, so
  the visitor feels them.
- **Banner:** a one-line banner under the strip says *"Demo: a recorded run, replayed. Nothing
  here reaches a cluster."* It links to the README.

### How it is built

| Part | Design |
|---|---|
| Player | `web/preview/replay.ts`: takes the recorded events and an injectable clock (`now`, `setTimer`), and applies each event through the same `views.apply` / `state.apply` path `full.ts` uses. The wait is `min(Δts, 10 s) / speed`, so a long idle gap does not stall the demo. Pause, speed and skip are methods. Stdlib vitest with a fake clock covers ordering, the cap, pause/resume, speed change mid-run and skip |
| Entry | `web/preview/demo.ts` and `demo.html`: `full.ts`'s mounting, with the player in place of the instant apply, the banner, and the inert sender. `full.ts` stays the developer preview |
| Paths | Every asset path in `demo.html` is relative (`app.css`, `demo.js`, `demo-session.jsonl`). GitHub Pages serves a project site under `/agent-platform/`, where `/src/app.css` would 404 |
| Recording | A curated `web/test/fixtures/demo-session.jsonl`, written by hand like `live-session.jsonl`, about 80–120 events of one plausible task. It includes three or more steps, a denied call, an approval requested then approved, a human steering message, a handoff to a reviewer, and a pull request link. No real repository content, secret or person. A vitest checks it is gapless, parses as the event envelope, and renders inert (the chat view's existing inert check) |
| Publish | A new workflow, `.github/workflows/demo.yaml`, runs on a release tag and on `workflow_dispatch`. It builds the demo (`npm ci`, an esbuild bundle, copying the HTML, CSS and fixture into `_site/`) and deploys it with `actions/upload-pages-artifact` and `actions/deploy-pages`, both pinned by SHA like the repository's other actions. Permissions: `pages: write`, `id-token: write`, `contents: read`. It runs no step on a pull request |
| One-time setup | The owner enables GitHub Pages (source: GitHub Actions) in the repository settings. The demo URL is `https://smana.github.io/agent-platform/` until part 2 renames it |

The demo is the same code as the product UI, so a UI change reaches the demo at the next release
with no second implementation. `ui:check` keeps the broker's bundle honest. The demo bundle is
built in CI and never committed.

## Verification

- **`hack/docs.sh`:** passes. No status vocabulary on reader pages, no dead link or anchor after
  the sections move.
- **Player tests:** fake clock, as above. The demo fixture test, as above.
- **Visual check:** `npm run preview` serves `demo.html` locally. Headless-chromium screenshots at
  10 s, 30 s and the end of the replay, light and dark, at 390 and 1280 px, attached to the PR.
- **The README's first screen:** a 1280×800 screenshot of the rendered README on the PR branch
  shows the headline, the three points and the demo link without scrolling.
- **Workflow:** `workflow_dispatch` on the branch after merge publishes the page. Its URL loads
  the demo, which plays to the end.
- **Gate:** `task check` exits 0.

## Out of scope

- The name and any rename (part 2).
- A portable `AgentRun`, the umbrella chart and the kind quickstart (part 3). The README's
  status names them as next.
- A docs site, analytics on the demo page, a video.
