# agent-platform documentation

<a href="naming.md">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/png/maku-lockup-horizontal-dark.png">
    <img src="assets/brand/png/maku-lockup-horizontal-light.png" alt="Maku, a controlled stage for delegated agent work" width="480">
  </picture>
</a>

Maku is the product name; `agent-platform` stays the name of the repository, module, binaries, images and APIs ([naming](naming.md)).

The reference for the broker, the bridge, the factory, the CLI and the web UI; the platform's docs site holds the overview and the status.

## Pages

| Page | Answers |
|---|---|
| [Quickstart](quickstart.md) | One command to a sandboxed run on a laptop, and what differs from production |
| [Concepts](concepts.md) | The vocabulary of rooms and the factory; the room's phases; the Room CRD |
| [Architecture](architecture.md) | The four binaries, the trust boundaries, one task's life, where each piece runs |
| [The factory](factory.md) | What a label starts, the Task lifecycle, triage and budgets, the merge gate, escalation, the kill switch; the Task CRD, the config file, the factory's API |
| [roomctl](roomctl.md) | Install and verify, configure, every command, the factory-handoff skill |
| [Web UI](web-ui.md) | The room list, the room page, what each standing may do |
| [Slack App](slack.md) | The factory's Slack App: manifest, config, the two flows, what it never does |
| [Event envelope](event-envelope.md) | The C4 envelope, its payloads and limits |
| [Room log](room-log.md) | The schema, the guarantees the database enforces, sealing, retention, migrations |
| [API](api.md) | The broker's four listeners, their errors and limits |
| [Security](security.md) | Threat model, identities, TLS, redaction, database roles, network, supply chain |
| [Integration](integration.md) | What this repo ships and what each consumer pins; the broker's config |
| [Operations](operations.md) | Metrics, alerts, SQL on the log, retention, backups, failures; the factory's signals |
| [Development](development.md) | The store test harness, migrations, the web build and preview |
| [Roadmap](roadmap.md) | What shipped, what is open |
| [Naming](naming.md) | Maku the product name, the unchanged technical names, the visual identity |

## Reading paths

| You are | Read |
|---|---|
| New here | [README](../README.md) → [Concepts](concepts.md) → [Architecture](architecture.md) |
| A maintainer with a factory issue or PR | [The factory](factory.md) → [Web UI](web-ui.md) → [Concepts](concepts.md) |
| A developer who wants the CLI | [roomctl](roomctl.md) → [Web UI](web-ui.md) |
| Operating a cluster | [Architecture](architecture.md) → [Integration](integration.md) → [chart README](../charts/agent-factory/README.md) → [Operations](operations.md) → [Security](security.md) |
| Changing the code | [AGENTS.md](../AGENTS.md) → [Development](development.md) → [Event envelope](event-envelope.md) → [Room log](room-log.md) → [API](api.md) |

Design sources: the [rooms design](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-09-23-agent-collaboration-rooms-design.md),
the [factory design](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-09-23-agent-dark-factory-design.md),
the [programme design](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-09-23-agent-factory-design.md)
and the [local-first UX design](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-10-08-agent-factory-local-first-ux-design.md).
The ruling ids in code comments point into their plans.
