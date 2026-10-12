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

var (
	externalSecretGVR = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}
	secretStoreGVR    = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}
)

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

// SecretStore odczytuje stan magazynu ESO z warunku Ready.
func (c *Cluster) SecretStore(ctx context.Context, namespace, name string) (cluster.SecretStore, error) {
	obj, err := c.dyn.Resource(secretStoreGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.SecretStore{}, err
	}
	var st cluster.SecretStore
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if m, ok := raw.(map[string]any); ok && m["type"] == "Ready" {
			st.Ready = m["status"] == "True"
			st.Reason, _ = m["reason"].(string)
			st.Message, _ = m["message"].(string)
		}
	}
	return st, nil
}

// RevalidateStore wymusza walidację magazynu ESO teraz. Nieudana walidacja
// (np. dostawca jeszcze nie istniał) jest ponawiana z narastającą przerwą,
// do kilkunastu minut, a każda zmiana obiektu wyzwala ją od razu — kontroler
// obserwuje SecretStore bez filtrów (ESO v2.12, secretstore_controller.go).
func (c *Cluster) RevalidateStore(ctx context.Context, namespace, name string, at time.Time) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{"docfind.io/revalidate": strconv.FormatInt(at.Unix(), 10)}}})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(secretStoreGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
