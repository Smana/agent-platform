# Roadmap

What shipped is the [releases page](https://github.com/Smana/agent-platform/releases); what is
proven live, and the hardening and expansions ahead, is the platform's
[status page](https://github.com/Smana/cloud-native-ref/blob/main/website/content/docs/platform/ai-platform/status.md).
This page lists what this repository still owes.

| Open item | Where it is tracked |
|---|---|
| OTel spans across bridge → broker → store, `traceparent` on every hop | `AGENTS.md`, Observability |
| The platform-agnostic decision after the UX sign-off: 2 of 4 triggers (daily use, AHP 1.0 without identity and audit, a second harness or runtime, outside demand) | `AGENTS.md`, Seams |
| A second repository for the factory: the App installed, the trust policies, the rulesets, more audiences, a multi-repository intake | the platform's agents overview, "One repository at first" |
| A two-pane room page: the task panel on the left, the stream and the dock on the right, two tabs on a phone, once usage says people live in the page | [Room readability, "Later: two panes"](superpowers/specs/2026-10-15-room-readability-design.md#later-two-panes) |
| TLS on the factory's run-request API, which serves plaintext on `:8082` inside the mesh | [Code health, "Open to the owner"](superpowers/specs/2026-10-15-code-health-design.md#open-to-the-owner) |
| A migration that drops `rooms.bridge_seen_at`, once every broker runs a release that no longer writes it | [Room log](room-log.md#schema) |
