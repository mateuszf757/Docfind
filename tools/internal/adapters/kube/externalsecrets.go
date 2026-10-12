package kube

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

var externalSecretGVR = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}

// ExternalSecret odczytuje stan synchronizacji ExternalSecret.
func (c *Cluster) ExternalSecret(ctx context.Context, namespace, name string) (cluster.ExternalSecret, error) {
	obj, err := c.dyn.Resource(externalSecretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.ExternalSecret{}, err
	}
	var st cluster.ExternalSecret
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if m, ok := raw.(map[string]any); ok && m["type"] == "Ready" {
			st.Ready = m["status"] == "True"
			st.Reason, _ = m["reason"].(string)
			st.Message, _ = m["message"].(string)
		}
	}
	if refreshed, ok, _ := unstructured.NestedString(obj.Object, "status", "refreshTime"); ok {
		st.RefreshTime, _ = time.Parse(time.RFC3339, refreshed)
	}
	return st, nil
}

// ForceSync wymusza synchronizację ExternalSecret teraz, a nie po
// refreshInterval — zmiana adnotacji force-sync (dokumentacja ESO).
func (c *Cluster) ForceSync(ctx context.Context, namespace, name string, at time.Time) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{"force-sync": strconv.FormatInt(at.Unix(), 10)}}})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(externalSecretGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
