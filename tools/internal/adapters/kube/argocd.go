package kube

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

var applicationGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// Application odczytuje stan aplikacji Argo CD: synchronizację z wersją,
// zdrowie, ostatnią operację i warunki.
func (c *Cluster) Application(ctx context.Context, namespace, name string) (cluster.Application, error) {
	obj, err := c.dyn.Resource(applicationGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.Application{}, fmt.Errorf("odczyt Application %s/%s: %w", namespace, name, err)
	}
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(obj.Object, path...)
		return v
	}
	app := cluster.Application{
		Sync:          str("status", "sync", "status"),
		Revision:      str("status", "sync", "revision"),
		Health:        str("status", "health", "status"),
		HealthMessage: str("status", "health", "message"),
		Operation: cluster.AppOperation{
			Phase:    str("status", "operationState", "phase"),
			Revision: str("status", "operationState", "syncResult", "revision"),
			Message:  str("status", "operationState", "message"),
		},
	}
	if op, ok := obj.Object["operation"]; ok && op != nil {
		app.Operation.Requested = true
	}
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if m, ok := raw.(map[string]any); ok {
			t, _ := m["type"].(string)
			msg, _ := m["message"].(string)
			app.Conditions = append(app.Conditions, cluster.AppCondition{Type: t, Message: msg})
		}
	}
	return app, nil
}

// RefreshApplication zleca Argo CD twarde odświeżenie aplikacji (adnotacja
// argocd.argoproj.io/refresh=hard, dokumentacja Argo CD): ponowne
// rozwiązanie zakresu wersji z pominięciem pamięci podręcznej listy tagów,
// zamiast czekać na następny cykl uzgadniania (do kilku minut).
func (c *Cluster) RefreshApplication(ctx context.Context, namespace, name string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{"argocd.argoproj.io/refresh": "hard"}}})
	if err != nil {
		return err
	}
	_, err = c.dyn.Resource(applicationGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// SyncApplication zleca Argo CD synchronizację Application do wersji
// revision — tę samą operację, którą zleca synchronizacja automatyczna
// (autoSync w controller/appcontroller.go, Argo CD 3.5.4): prune i opcje
// z syncPolicy, ponowienia z syncPolicy.retry, bez niej limit 5. Zapis
// z wersją obiektu: gdy Argo w międzyczasie zmienił Application, API
// zwraca konflikt zamiast nadpisać jego operację; operacja w toku — błąd.
func (c *Cluster) SyncApplication(ctx context.Context, namespace, name, revision string) error {
	res := c.dyn.Resource(applicationGVR).Namespace(namespace)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("odczyt Application %s/%s: %w", namespace, name, err)
	}
	if op, ok := obj.Object["operation"]; ok && op != nil {
		return fmt.Errorf("Application %s/%s ma operację w toku", namespace, name)
	}
	prune, _, _ := unstructured.NestedBool(obj.Object, "spec", "syncPolicy", "automated", "prune")
	sync := map[string]any{"revision": revision, "prune": prune}
	if options, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "syncPolicy", "syncOptions"); len(options) > 0 {
		list := make([]any, len(options))
		for i, o := range options {
			list[i] = o
		}
		sync["syncOptions"] = list
	}
	retry, found, _ := unstructured.NestedMap(obj.Object, "spec", "syncPolicy", "retry")
	if !found {
		retry = map[string]any{"limit": int64(5)}
	}
	obj.Object["operation"] = map[string]any{
		"initiatedBy": map[string]any{"username": "dft"},
		"sync":        sync,
		"retry":       retry,
	}
	if _, err := res.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("synchronizacja Application %s/%s: %w", namespace, name, err)
	}
	return nil
}

// HelmReleaseRecords zwraca Secrety, w których Helm trzyma historię wydania
// release w przestrzeni nazw (etykiety owner=helm, name=<wydanie>) — tylko
// zapis wydania, nie zasoby, które Helm zainstalował.
func (c *Cluster) HelmReleaseRecords(ctx context.Context, namespace, release string) ([]string, error) {
	list, err := c.core.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + release})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range list.Items {
		names = append(names, s.Name)
	}
	return names, nil
}

// DeleteSecret usuwa Secret; Secret, którego już nie ma, to nie błąd.
func (c *Cluster) DeleteSecret(ctx context.Context, namespace, name string) error {
	err := c.core.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
