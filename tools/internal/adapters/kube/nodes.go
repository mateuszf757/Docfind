package kube

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

// Nodes zwraca węzły klastra.
func (c *Cluster) Nodes(ctx context.Context) ([]cluster.Node, error) {
	list, err := c.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodes := make([]cluster.Node, 0, len(list.Items))
	for _, n := range list.Items {
		ready := false
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ready = cond.Status == corev1.ConditionTrue
			}
		}
		nodes = append(nodes, cluster.Node{Name: n.Name, Ready: ready, Unschedulable: n.Spec.Unschedulable, Annotations: n.Annotations})
	}
	return nodes, nil
}

// Cordon odcina węzeł od nowych podów i zapisuje na nim adnotacje.
func (c *Cluster) Cordon(ctx context.Context, node string, annotations map[string]string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": annotations},
		"spec":     map[string]any{"unschedulable": true},
	}
	if err := c.patchNode(ctx, node, patch); err != nil {
		return fmt.Errorf("cordon %s: %w", node, err)
	}
	return nil
}

// Uncordon przywraca węzeł i usuwa podane adnotacje.
func (c *Cluster) Uncordon(ctx context.Context, node string, annotations []string) error {
	remove := map[string]any{}
	for _, a := range annotations {
		remove[a] = nil
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": remove},
		"spec":     map[string]any{"unschedulable": nil},
	}
	if err := c.patchNode(ctx, node, patch); err != nil {
		return fmt.Errorf("uncordon %s: %w", node, err)
	}
	return nil
}

func (c *Cluster) patchNode(ctx context.Context, node string, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.core.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}
