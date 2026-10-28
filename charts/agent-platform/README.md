# agent-platform chart

The umbrella chart: the room broker, the run controller (`runController`), the `Room` and `AgentRun`
CRDs (`crds/`), and the runs namespace with its default-deny policy. It has no factory: install
[agent-factory](../agent-factory/README.md) on its own.

## Values that matter

| Value | Meaning |
|---|---|
| `runs.{group,namespace,namePrefix}` | The only source of the run kind: the broker's `runs:` and its run-issuer `subPattern` both render from it |
| `broker.image.tag` | `<version>@sha256:<digest>`; a bare tag is refused unless `e2e.unpinnedImages` |
| `broker.publicURL` | `oidc`: the https URL humans use. `local`: `http://localhost:<forwarded port>` (default `:8080`); the broker answers that port only, under `localhost`, `127.0.0.1` or `[::1]` |
| `broker.human.mode` | `oidc` (block passed through verbatim) or `local` (one loopback user; `:8080` is not exposed, use a port-forward) |
| `broker.runIssuer.mode` | `jwks` (`issuer`, `jwksURL`) or `tokenreview` (binds `system:auth-delegator`) |
| `broker.roomTools.enabled` | The agents' `:8090` room tools listener. On by default: the block absent, or `enabled` unset, means on. An explicit `false` (the quickstart's laptop profile) renders `roomTools.enabled: false`, and the broker binds no `:8090` at all. On, `ROOMS_MCP_KEY` alone decides whether a call is accepted, so supply it through `broker.extraEnv`; unset, every call is refused `401` |
| `broker.database.brokerSecret` / `ownerSecret` | Secrets with key `uri`. An `ownerSecret` (the `rooms_owner` DSN) adds the `migrate` init container, which reads it as `ROOMS_MIGRATION_DATABASE_URL`; the broker reads `brokerSecret` as `ROOMS_DATABASE_URL`. An empty `brokerSecret` omits `ROOMS_DATABASE_URL`: supply it through `broker.extraEnv` |
| `broker.tls.issuerRef.name` | Renders a cert-manager `Certificate` for `broker.tls.secretName`; empty = bring the Secret yourself |

`values-quickstart.yaml` is the laptop profile: local humans, tokenreview, a namespaced `Issuer`.

```bash
helm template ap charts/agent-platform -n agent-platform -f charts/agent-platform/values-quickstart.yaml \
  --set broker.image.tag=<version>@sha256:<digest> --set runController.image.tag=<version>@sha256:<digest>
```

## Caveats

| Topic | Behaviour |
|---|---|
| CRDs | `crds/` are installed once and **not upgraded by Helm**: `kubectl apply -f charts/agent-platform/crds` on every upgrade |
| Runs namespace | Kept on uninstall (`helm.sh/resource-policy: keep`) and keeps its Helm ownership annotations: reinstalling under another release name needs `createRunsNamespace: false` |
| `:8090` | `broker.roomTools.enabled: false` binds nothing. On, the port is not modelled in the chart: no Service port and no policy for it, so the `agent-router` route and its ingress are yours |
| `oidc` mode network | Needs `broker.networkPolicy.extraEgress` (JWKS) and `extraIngress` (`:8080` from the proxy, `:9090` scraping). Default-deny leaves `:9090` unscrapable for both the broker and the controller |
| `createRuntimeClass` | Off by default (the RuntimeClass name is cluster-scoped); on in the quickstart |
| Broker runtime | The broker pod never takes a runtimeClassName (refused at render): kubectl port-forward cannot reach a runsc pod |
| `apiServerPorts` | `[443, 6443]`: the ports the egress policy opens to the apiserver |

Checks: `task chart:check`. The laptop install is the [quickstart](../../docs/quickstart.md).
