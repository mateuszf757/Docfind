package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

var (
	gatewayGVR     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	httpRouteGVR   = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
)

// Gateway odczytuje Gateway: nazwę hosta listenera https, wydawcę
// z adnotacji cert-managera i warunek Programmed.
func (c *Cluster) Gateway(ctx context.Context, namespace, name string) (cluster.Gateway, error) {
	gw, err := c.dyn.Resource(gatewayGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.Gateway{}, fmt.Errorf("odczyt Gateway %s/%s: %w", namespace, name, err)
	}
	out := cluster.Gateway{Issuer: gw.GetAnnotations()["cert-manager.io/cluster-issuer"], Programmed: conditionTrue(gw, "Programmed")}
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	for _, l := range listeners {
		if m, ok := l.(map[string]any); ok && m["name"] == "https" {
			out.HTTPSHostname, _ = m["hostname"].(string)
		}
	}
	return out, nil
}

// Route odczytuje warunki Accepted i ResolvedRefs nadane HTTPRoute przez
// Gateway („brak", gdy jeszcze ich nie ma).
func (c *Cluster) Route(ctx context.Context, namespace, name string) (cluster.Route, error) {
	route, err := c.dyn.Resource(httpRouteGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.Route{}, fmt.Errorf("HTTPRoute %s/%s: %w", namespace, name, err)
	}
	status := cluster.Route{Accepted: "brak", ResolvedRefs: "brak"}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		conditions, _ := pm["conditions"].([]any)
		for _, raw := range conditions {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			value, _ := m["status"].(string)
			switch m["type"] {
			case "Accepted":
				status.Accepted = value
			case "ResolvedRefs":
				status.ResolvedRefs = value
			}
		}
	}
	return status, nil
}

// Certificate odczytuje rewizję i gotowość certyfikatu cert-managera.
func (c *Cluster) Certificate(ctx context.Context, namespace, name string) (cluster.Certificate, error) {
	cert, err := c.dyn.Resource(certificateGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.Certificate{}, err
	}
	rev, _, _ := unstructured.NestedInt64(cert.Object, "status", "revision")
	return cluster.Certificate{Revision: rev, Ready: conditionTrue(cert, "Ready")}, nil
}

func conditionTrue(obj *unstructured.Unstructured, condition string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if m, ok := raw.(map[string]any); ok && m["type"] == condition {
			return m["status"] == "True"
		}
	}
	return false
}
