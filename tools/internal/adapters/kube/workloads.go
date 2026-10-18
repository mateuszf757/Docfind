package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

// Pods zwraca pody przestrzeni nazw pasujące do selektora etykiet
// (pusty — wszystkie).
func (c *Cluster) Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error) {
	list, err := c.core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return toPods(list.Items), nil
}

// PodsOnNode zwraca pody wszystkich przestrzeni nazw na węźle.
func (c *Cluster) PodsOnNode(ctx context.Context, node string) ([]cluster.Pod, error) {
	list, err := c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, err
	}
	return toPods(list.Items), nil
}

func toPods(items []corev1.Pod) []cluster.Pod {
	pods := make([]cluster.Pod, 0, len(items))
	for _, p := range items {
		pod := cluster.Pod{
			Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Labels: p.Labels,
			Images:      map[string]string{},
			Running:     p.Status.Phase == corev1.PodRunning,
			Terminating: p.DeletionTimestamp != nil,
			UID:         string(p.UID),
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady {
				pod.Ready = cond.Status == corev1.ConditionTrue
			}
		}
		for _, ctr := range p.Spec.Containers {
			pod.Images[ctr.Name] = ctr.Image
		}
		for _, s := range p.Status.ContainerStatuses {
			pod.Restarts += s.RestartCount
			if s.RestartCount > 0 {
				pod.Restarted = append(pod.Restarted, s.Name)
			}
		}
		pods = append(pods, pod)
	}
	return pods
}

// CreatePod tworzy pod.
func (c *Cluster) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	_, err := c.core.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

// PodLog zwraca log kontenera poda.
func (c *Cluster) PodLog(ctx context.Context, namespace, pod, container string) ([]byte, error) {
	return c.core.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
}

// DeletePod usuwa pod; brak poda to nie błąd.
func (c *Cluster) DeletePod(ctx context.Context, namespace, pod string) error {
	err := c.core.CoreV1().Pods(namespace).Delete(ctx, pod, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ProxyGet czyta ścieżkę HTTP poda przez proxy API servera — trafia
// dokładnie we wskazany pod, nie w Service, i nie zależy od zawartości
// obrazu (powłoki, interpretera), jak `kubectl exec`.
func (c *Cluster) ProxyGet(ctx context.Context, namespace, pod, port, path string) ([]byte, error) {
	return c.core.CoreV1().Pods(namespace).ProxyGet("http", pod, port, path, nil).DoRaw(ctx)
}

// Rollout zwraca stan Deploymentu.
func (c *Cluster) Rollout(ctx context.Context, namespace, name string) (cluster.Rollout, error) {
	dep, err := c.core.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.Rollout{}, fmt.Errorf("odczyt Deploymentu %s/%s: %w", namespace, name, err)
	}
	if dep.Spec.Selector == nil {
		return cluster.Rollout{}, fmt.Errorf("deployment %s/%s bez selektora", namespace, name)
	}
	wanted := int32(1)
	if dep.Spec.Replicas != nil {
		wanted = *dep.Spec.Replicas
	}
	s := dep.Status
	return cluster.Rollout{
		Wanted: wanted, Updated: s.UpdatedReplicas, Available: s.AvailableReplicas, Total: s.Replicas,
		Observed:       s.ObservedGeneration >= dep.Generation,
		Selector:       labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
		TemplateLabels: dep.Spec.Template.Labels,
	}, nil
}

// Deployments zwraca Deploymenty pasujące do selektora jako manifesty —
// do sprawdzenia politykami (core/policy) zasobów, które tworzy kontroler
// w trakcie działania i których nie ma w `helm template`.
func (c *Cluster) Deployments(ctx context.Context, namespace, selector string) ([]map[string]any, error) {
	list, err := c.core.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var manifests []map[string]any
	for _, d := range list.Items {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		// Obiekt z listy nie ma apiVersion i kind — polityki rozpoznają
		// rodzaj zasobu po kind.
		m["kind"] = "Deployment"
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// PDBFor zwraca PodDisruptionBudget, którego selektor obejmuje pody
// o podanych etykietach.
func (c *Cluster) PDBFor(ctx context.Context, namespace string, podLabels map[string]string) (cluster.PDB, bool, error) {
	pdbs, err := c.core.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return cluster.PDB{}, false, err
	}
	for _, pdb := range pdbs.Items {
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || !sel.Matches(labels.Set(podLabels)) {
			continue
		}
		return cluster.PDB{Name: pdb.Name, DisruptionsAllowed: pdb.Status.DisruptionsAllowed}, true, nil
	}
	return cluster.PDB{}, false, nil
}

// EnsureNamespace zakłada przestrzeń nazw z etykietami albo dopisuje
// etykiety do istniejącej.
func (c *Cluster) EnsureNamespace(ctx context.Context, name string, nsLabels map[string]string) error {
	_, err := c.core.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nsLabels}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	if len(nsLabels) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": nsLabels}})
	if err != nil {
		return err
	}
	_, err = c.core.CoreV1().Namespaces().Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// ScaleDeployment ustawia liczbę replik Deploymentu — zmiana spec.replicas,
// jak `kubectl scale`.
func (c *Cluster) ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"replicas": replicas}})
	if err != nil {
		return err
	}
	if _, err := c.core.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("skalowanie Deploymentu %s/%s: %w", namespace, name, err)
	}
	return nil
}

// PodDisruptionBudgetExists mówi, czy PDB o tej nazwie istnieje.
func (c *Cluster) PodDisruptionBudgetExists(ctx context.Context, namespace, name string) (bool, error) {
	_, err := c.core.PolicyV1().PodDisruptionBudgets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// DeletePodDisruptionBudget usuwa PDB.
func (c *Cluster) DeletePodDisruptionBudget(ctx context.Context, namespace, name string) error {
	return c.core.PolicyV1().PodDisruptionBudgets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// pullingImage — komunikat zdarzenia Pulling kubeleta.
var pullingImage = regexp.MustCompile(`Pulling image "([^"]+)"`)

// PullEvents zwraca obrazy ze zdarzeń Pulling we wszystkich przestrzeniach
// nazw — co kubelet pobierał z sieci, póki zdarzenia nie wygasły (domyślnie
// godzina).
func (c *Cluster) PullEvents(ctx context.Context) ([]string, error) {
	list, err := c.core.CoreV1().Events("").List(ctx, metav1.ListOptions{FieldSelector: "reason=Pulling"})
	if err != nil {
		return nil, err
	}
	var images []string
	for _, e := range list.Items {
		if m := pullingImage.FindStringSubmatch(e.Message); m != nil && !slices.Contains(images, m[1]) {
			images = append(images, m[1])
		}
	}
	return images, nil
}
