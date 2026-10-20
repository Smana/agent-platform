# agent-platform chart

The umbrella chart: the room broker, the `Room` and `AgentRun` CRDs (`crds/`), the runs
namespace with its default-deny policy, and — behind `factory.enabled` — the
[agent-factory](../agent-factory/README.md) subchart. The run controller is `runController`.

## Values that matter

| Value | Meaning |
|---|---|
| `runs.{group,namespace,namePrefix}` | The only source of the run kind: the broker's `runs:` and its run-issuer `subPattern` both render from it |
| `broker.image.tag` | `<version>@sha256:<digest>`; a bare tag is refused unless `e2e.unpinnedImages` |
| `broker.human.mode` | `oidc` (block passed through verbatim) or `local` (one loopback user; `:8080` is not exposed, use a port-forward) |
| `broker.runIssuer.mode` | `jwks` (`issuer`, `jwksURL`) or `tokenreview` (binds `system:auth-delegator`) |
| `broker.database.brokerSecret` / `ownerSecret` | Secrets with key `uri`. An `ownerSecret` adds the `migrate` init container. An empty `brokerSecret` omits `ROOMS_DATABASE_URL`: supply it through `broker.extraEnv` |
| `broker.tls.issuerRef.name` | Renders a cert-manager `Certificate` for `broker.tls.secretName`; empty = bring the Secret yourself |

`values-quickstart.yaml` is the laptop profile: local humans, tokenreview, a namespaced `Issuer`.

```bash
helm dependency build charts/agent-platform
helm template ap charts/agent-platform -n agent-platform -f charts/agent-platform/values-quickstart.yaml \
  --set broker.image.tag=<version>@sha256:<digest> --set runController.image.tag=<version>@sha256:<digest>
```

## Caveats

| Topic | Behaviour |
|---|---|
| CRDs | `crds/` are installed once and **not upgraded by Helm**: `kubectl apply -f charts/agent-platform/crds` on every upgrade |
| Runs namespace | Kept on uninstall (`helm.sh/resource-policy: keep`) and keeps its Helm ownership annotations: reinstalling under another release name needs `createRunsNamespace: false` |
| `:8090` | The agents' room tools listener is not modelled in the chart (no Service port, no policy) |
| `oidc` mode network | Needs `broker.networkPolicy.extraEgress` (JWKS) and `extraIngress` (`:8080` from the proxy, `:9090` scraping). Default-deny leaves `:9090` unscrapable for both the broker and the controller |
| `createRuntimeClass` | Off by default (the RuntimeClass name is cluster-scoped); on in the quickstart |
| `apiServerPorts` | `[443, 6443]`: the ports the egress policy opens to the apiserver |

Checks: `task chart:check`. The laptop install is the [quickstart](../../docs/quickstart.md).
