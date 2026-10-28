// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/Smana/maku/api/v1alpha1"
)

// NetworkPolicy is the run's default-deny CNP: DNS for its names only, the broker on 8443,
// github and its profiles on 443, the model host on its port. Nothing dials in except the
// kubelet's probe of the bridge (C4).
func NetworkPolicy(r *v1alpha1.AgentRun, c Config, m Model, runID string) *unstructured.Unstructured {
	web := c.FQDNs(r) // reached on 443
	modelNS, inCluster := inClusterNamespace(m.Host)
	if m.Port == 443 && !inCluster {
		web = addUnique(web, m.Host)
	}
	names := addUnique(slices.Clone(web), m.Host)
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
	// A bare ingress section is what puts the endpoint in ingress default-deny; Cilium documents
	// the empty rule {} for it, and a Go-empty list is omitted by omitempty, so it would not.
	ingress := []any{map[string]any{}}
	var roomEgress []any
	if r.Spec.RoomRef != "" {
		names = append(names, brokerHost(c))
		sel := map[string]any{"io.kubernetes.pod.namespace": c.Broker.Namespace}
		for k, v := range c.Broker.Labels {
			sel[k] = v
		}
		roomEgress = []any{map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": sel}}, "toPorts": port(8443)}}
		ingress = []any{map[string]any{"fromEntities": []any{"host"}, "toPorts": port(BridgeHealthPort)}}
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
	switch {
	case inCluster:
		// Not toFQDNs: that matches the DNS-resolved ClusterIP, but Cilium translates the Service to a
		// backend pod before policy, so the rule would never match. Policy sees the backend port: the model
		// URL's port must equal the Service's targetPort.
		sel := map[string]any{"io.kubernetes.pod.namespace": modelNS}
		egress = append(egress, map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": sel}}, "toPorts": port(m.Port)})
	case m.Port != 443:
		egress = append(egress, map[string]any{"toFQDNs": matchNames([]string{m.Host}), "toPorts": port(m.Port)})
	}
	egress = append(egress, roomEgress...)
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

// inClusterNamespace is the namespace of a Service host (<svc>.<ns>.svc or <svc>.<ns>.svc.<domain>),
// the only hosts ModelFrom lets use plain http.
func inClusterNamespace(host string) (string, bool) {
	p := strings.Split(host, ".")
	if len(p) >= 3 && p[2] == "svc" && p[0] != "" && p[1] != "" {
		return p[1], true
	}
	return "", false
}
