package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// EnsureConfigMap zakłada ConfigMapę z danymi albo poprawia jej dane, jeśli
// się różnią.
func (c *Cluster) EnsureConfigMap(ctx context.Context, namespace, name string, data map[string]string) error {
	cms := c.core.CoreV1().ConfigMaps(namespace)
	cm, err := cms.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "dft"}},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if maps.Equal(cm.Data, data) {
		return nil
	}
	cm.Data = data
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// mapping zamienia apiVersion i kind na zasób API (discovery, w pamięci
// podręcznej adaptera).
func (c *Cluster) mapping(apiVersion, kind string) (*meta.RESTMapping, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}
	if c.mapper == nil {
		c.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(c.core.Discovery()))
	}
	return c.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
}

// RemoveFieldManager usuwa z managedFields zasobu wpisy menedżera manager,
// jeśli zasobem zarządza też keep — po przejęciu wydania Helma przez Argo CD
// (manager „helm", keep „argocd-controller"). Bez tego pola, które Helm
// zastosował przez Server-Side Apply, zostają jego własnością i nie znikają,
// gdy znikną z charta, który stosuje Argo. namespace — przestrzeń wydania
// dla zasobu bez jawnej przestrzeni. Zwraca, czy coś zmienił; zasobu,
// którego nie ma, nie zmienia.
func (c *Cluster) RemoveFieldManager(ctx context.Context, apiVersion, kind, namespace, name, manager, keep string) (bool, error) {
	m, err := c.mapping(apiVersion, kind)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", kind, name, err)
	}
	var ri dynamic.ResourceInterface = c.dyn.Resource(m.Resource)
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = c.dyn.Resource(m.Resource).Namespace(namespace)
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var kept []metav1.ManagedFieldsEntry
	removed, keeps := false, false
	for _, f := range obj.GetManagedFields() {
		switch f.Manager {
		case manager:
			removed = true
			continue
		case keep:
			keeps = true
		}
		kept = append(kept, f)
	}
	if !removed || !keeps {
		return false, nil
	}
	patch, err := json.Marshal([]map[string]any{{"op": "replace", "path": "/metadata/managedFields", "value": kept}})
	if err != nil {
		return false, err
	}
	if _, err := ri.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("%s %s: %w", kind, name, err)
	}
	return true, nil
}
