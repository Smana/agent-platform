# agent-platform documentation

These pages explain what the room broker and the room bridge do, how they are built, how they plug
into the platform, and how to run and change them. **Most of it is not built yet**: every section
says whether it describes code on `main`, code under review, or a plan, and names the phase that
delivers it.

## Status tags

| Tag | Meaning |
|---|---|
| **shipped** | Merged on `main` |
| **AP-1** | Written and tested on the phase-1 branch (`feat/room-log`), not merged yet |
| **AP-1, Ruling Y** | Designed and being implemented on the phase-1 branch (see [room log](room-log.md#the-guarantees)) |
| **planned, phase N / AP-N** | Specified in the design and plan, not written. `AP-N` is the pull request in this repository that delivers it |

Phases follow the implementation plan's numbering, which differs from the design's: see
[roadmap](roadmap.md#phase-numbering).

## Pages

| Page | Answers |
|---|---|
| [Concepts](concepts.md) | What a room, run, driver, handoff, seal, lease or envelope is |
| [Architecture](architecture.md) | Components, trust boundaries, one run's life, where each piece is deployed and which repository owns it |
| [Event envelope](event-envelope.md) | The C4 envelope every log entry uses, its payloads, limits and an example of each event type |
| [Room log](room-log.md) | The Postgres schema, the guarantees the database enforces, sealing, retention, the bridge lease and Atlas migrations |
| [API](api.md) | The `:8443` bridge and system API, the `:8080` human API and the `:8090` room tools, with their errors and limits |
| [Security](security.md) | Threat model, identities, TLS, redaction, database roles, network policy and the supply chain |
| [Integration](integration.md) | What cloud-native-ref and crossplane-configuration deploy, and which pre-release pins which |
| [Operations](operations.md) | Metrics, alerts, retention, backups, reading the log with SQL, common failures, upgrades |
| [Development](development.md) | Toolchain, `task check`, tests, migrations, lint, releases, conventions |
| [Roadmap](roadmap.md) | Phases 0 to 7, their pull requests across the three repositories, and where each stands |

## Reading order

| You are | Read |
|---|---|
| New to the project | [Concepts](concepts.md) → [Architecture](architecture.md) → [Roadmap](roadmap.md) |
| Operating a cluster | [Architecture](architecture.md) → [Integration](integration.md) → [Operations](operations.md) → [Security](security.md) |
| Changing the code | [Development](development.md) → [Event envelope](event-envelope.md) → [Room log](room-log.md) → [API](api.md) → [Security](security.md) |

## Sources

This guide restates, it does not decide. Where the sources disagree it follows the plan, then the
plan's rulings, then the amendments recorded while executing it.

| Source | Holds |
|---|---|
| [SP2 design: collaboration rooms](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-09-23-agent-collaboration-rooms-design.md) | The room model, protocol, security model, Appendix A payloads and Appendix C roles |
| [Agent Factory programme design](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/specs/2026-09-23-agent-factory-design.md) | Contracts C1–C7, including C4, the event envelope |
| [SP2 implementation plan](https://github.com/Smana/cloud-native-ref/blob/main/docs/superpowers/plans/2026-09-27-agent-collaboration-rooms-plan.md) | Tasks, the pre-flight rulings P1–P40 and the PR map |
| Amendments applied while executing | **GP-18**: TLS on `:8443` on both clouds. **Ruling Y**: the database-enforced guarantees of the log. **P33 lifted for this repository**: its pull requests merge to `main` when green. The plan on `main` predates the first two |
