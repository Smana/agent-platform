// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

// NetworkPolicy is the run's default-deny CNP: DNS for its names only, the broker on 8443,
// github and its profiles on 443, the model host on its port. Nothing dials in except the
// kubelet's probe of the bridge (C4).
func NetworkPolicy(r *v1alpha1.AgentRun, c Config, m Model, runID string) *unstructured.Unstructured {
	web := c.FQDNs(r) // reached on 443
	if m.Port == 443 && !slices.Contains(web, m.Host) {
		web = append(web, m.Host)
	}
	names := slices.Clone(web)
	if !slices.Contains(names, m.Host) {
		names = append(names, m.Host)
	}
	if r.Spec.RoomRef != "" {
		names = append(names, brokerHost(c))
	}
	matchNames := func(hs []string) []any {
		out := make([]any, 0, len(hs))
		for _, h := range hs {
			out = append(out, map[string]any{"matchName": h})
		}
		return out
	}
	port := func(p int) []any {
		return []any{map[string]any{"ports": []any{map[string]any{"port": strconv.Itoa(p), "protocol": "TCP"}}}}
	}
	egress := []any{
		map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{"io.kubernetes.pod.namespace": "kube-system", "k8s-app": "kube-dns"}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": "53", "protocol": "UDP"}, map[string]any{"port": "53", "protocol": "TCP"}},
				"rules": map[string]any{"dns": matchNames(names)},
			}},
		},
		map[string]any{"toFQDNs": matchNames(web), "toPorts": port(443)},
	}
	if m.Port != 443 {
		egress = append(egress, map[string]any{"toFQDNs": matchNames([]string{m.Host}), "toPorts": port(m.Port)})
	}
	// A bare ingress section is what puts the endpoint in ingress default-deny; Cilium documents
	// the empty rule {} for it, and a Go-empty list is omitted by omitempty, so it would not.
	ingress := []any{map[string]any{}}
	if r.Spec.RoomRef != "" {
		sel := map[string]any{"io.kubernetes.pod.namespace": c.Broker.Namespace}
		for k, v := range c.Broker.Labels {
			sel[k] = v
		}
		egress = append(egress, map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": sel}}, "toPorts": port(8443)})
		ingress = []any{map[string]any{"fromEntities": []any{"host"}, "toPorts": port(BridgeHealthPort)}}
	}
	spec := map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{LabelRunID: runID}},
		"egress":           egress,
		"ingress":          ingress,
	}
	u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy", "spec": spec}}
	om := meta(r, runID, r.Name)
	u.SetName(om.Name)
	u.SetNamespace(om.Namespace)
	u.SetLabels(om.Labels)
	return u
}
