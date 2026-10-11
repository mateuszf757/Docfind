package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Drain opróżnia węzeł przez `kubectl drain`: eksmisja z poszanowaniem
// PodDisruptionBudget, z pominięciem DaemonSetów.
func (c *Cluster) Drain(ctx context.Context, node string, timeout time.Duration) error {
	args := append([]string{"drain", node, "--ignore-daemonsets", "--delete-emptydir-data", fmt.Sprintf("--timeout=%s", timeout)}, c.kubectlArgs()...)
	if _, err := c.runner.Run(ctx, proc.Cmd{Name: "kubectl", Args: args}); err != nil {
		return fmt.Errorf("kubectl drain %s: %w", node, err)
	}
	return nil
}

// RenewCertificate wymusza odnowienie certyfikatu cert-managera (`cmctl
// renew`) — tak, jak zrobiłby to cert-manager przed wygaśnięciem.
func (c *Cluster) RenewCertificate(ctx context.Context, namespace, name string) error {
	args := append([]string{"renew", "--namespace", namespace, name}, c.kubectlArgs()...)
	if _, err := c.runner.Run(ctx, proc.Cmd{Name: "cmctl", Args: args}); err != nil {
		return fmt.Errorf("cmctl renew: %w", err)
	}
	return nil
}

// DeploymentImage zwraca obraz kontenera w szablonie poda Deploymentu.
func (c *Cluster) DeploymentImage(ctx context.Context, namespace, name, container string) (string, error) {
	dep, err := c.core.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("odczyt Deploymentu %s/%s: %w", namespace, name, err)
	}
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		if ctr.Name == container {
			return ctr.Image, nil
		}
	}
	return "", fmt.Errorf("deployment %s/%s bez kontenera %s", namespace, name, container)
}

// DeploymentEnv zwraca wartość zmiennej środowiskowej kontenera w szablonie
// poda Deploymentu; found — zmienna jest w specyfikacji (choćby pusta).
func (c *Cluster) DeploymentEnv(ctx context.Context, namespace, name, container, key string) (value string, found bool, err error) {
	dep, err := c.core.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", false, fmt.Errorf("odczyt Deploymentu %s/%s: %w", namespace, name, err)
	}
	for _, ctr := range dep.Spec.Template.Spec.Containers {
		if ctr.Name != container {
			continue
		}
		for _, e := range ctr.Env {
			if e.Name == key {
				return e.Value, true, nil
			}
		}
		return "", false, nil
	}
	return "", false, fmt.Errorf("deployment %s/%s bez kontenera %s", namespace, name, container)
}

// Zrzuty stanu klastra do dowodów — w postaci, którą czyta człowiek
// (kubectl get/describe/logs), a nie w obiektach API.

// DumpNodes zapisuje `kubectl get nodes -o wide`.
func (c *Cluster) DumpNodes(ctx context.Context, w io.Writer) error {
	return c.dump(ctx, w, "get", "nodes", "-o", "wide")
}

// DumpPods zapisuje pody wszystkich przestrzeni nazw.
func (c *Cluster) DumpPods(ctx context.Context, w io.Writer) error {
	return c.dump(ctx, w, "get", "pods", "-A", "-o", "wide")
}

// DumpEvents zapisuje zdarzenia wszystkich przestrzeni nazw, od najstarszego.
func (c *Cluster) DumpEvents(ctx context.Context, w io.Writer) error {
	return c.dump(ctx, w, "get", "events", "-A", "--sort-by=.lastTimestamp")
}

// DumpPlatform zapisuje zasoby wejścia i certyfikatów: Gateway API
// i cert-managera z wyzwaniami ACME.
func (c *Cluster) DumpPlatform(ctx context.Context, w io.Writer) error {
	return c.dump(ctx, w, "get", "gateways,httproutes,certificates,certificaterequests,orders,challenges,clusterissuers", "-A", "-o", "yaml")
}

// DescribePods zapisuje `kubectl describe pods` przestrzeni nazw.
func (c *Cluster) DescribePods(ctx context.Context, namespace string, w io.Writer) error {
	return c.dump(ctx, w, "describe", "pods", "-n", namespace)
}

// Logs zapisuje logi poda z czasami: jednego kontenera albo — gdy container
// pusty — wszystkich. previous — log poprzedniej instancji po restarcie;
// tylko dla jednego kontenera, bo `--previous --all-containers` zawodzi,
// gdy któryś kontener poda się nie restartował, i gubi log tego, który
// się restartował.
func (c *Cluster) Logs(ctx context.Context, namespace, pod, container string, previous bool, w io.Writer) error {
	args := []string{"logs", "-n", namespace, pod, "--prefix", "--timestamps"}
	if container == "" {
		args = append(args, "--all-containers")
	} else {
		args = append(args, "-c", container)
	}
	if previous {
		args = append(args, "--previous")
	}
	return c.dump(ctx, w, args...)
}

func (c *Cluster) dump(ctx context.Context, w io.Writer, args ...string) error {
	_, err := c.runner.Run(ctx, proc.Cmd{Name: "kubectl", Args: append(args, c.kubectlArgs()...), Stdout: w, Stderr: w})
	return err
}

// Kubectl — lokalny program kubectl, bez klastra.
type Kubectl struct {
	Runner proc.Runner
}

// ClientVersion zwraca wersję kubectl (np. v1.36.4).
func (k Kubectl) ClientVersion(ctx context.Context) (string, error) {
	out, err := proc.Output(ctx, k.Runner, proc.Cmd{Name: "kubectl", Args: []string{"version", "--client", "-o", "json"}})
	if err != nil {
		return "", fmt.Errorf("kubectl version: %w", err)
	}
	var version struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal([]byte(out), &version); err != nil {
		return "", fmt.Errorf("kubectl version: nieczytelny JSON: %w", err)
	}
	if version.ClientVersion.GitVersion == "" {
		return "", fmt.Errorf("kubectl version: brak clientVersion.gitVersion")
	}
	return strings.TrimSpace(version.ClientVersion.GitVersion), nil
}
