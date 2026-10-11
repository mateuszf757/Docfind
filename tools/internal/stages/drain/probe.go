package drain

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/ociarchive"
)

// ProbeRepository — nazwa obrazu sondy. Obraz nie trafia do rejestru:
// budowany ze źródeł tego commita i importowany do węzłów.
const ProbeRepository = "docfind-drain-probe"

// ProbeUser — UID i GID sondy. Obraz nie ma /etc/passwd, a runAsNonRoot
// sprawdza tylko numeryczny UID, więc użytkownik jest liczbą (65532 —
// konwencja „nonroot" obrazów distroless).
const ProbeUser = 65532

// GoBuilder — build programu Go (adapters/gotool).
type GoBuilder interface {
	Build(ctx context.Context, pkg, output string, env ...string) error
}

// Importer — import obrazu do węzłów klastra k3d (adapters/k3d).
type Importer interface {
	Import(ctx context.Context, cluster, image string) error
}

// ProbeImage przygotowuje obraz sondy na klastrze: binarka ze źródeł tego
// commita dla architektury węzłów (tej samej co host, bo węzły k3d to
// kontenery na tym hoście), obraz z jedną warstwą i import do węzłów.
type ProbeImage struct {
	Go       GoBuilder
	Importer Importer
	// Cluster — nazwa klastra k3d.
	Cluster string
}

// Prepare buduje i importuje sondę; zwraca jej obraz:tag. Tag z digestu
// obrazu: ta sama binarka — ten sam tag, więc ponowny import do węzłów nic
// nie zmienia.
func (p ProbeImage) Prepare(ctx context.Context, tmp string) (string, error) {
	binary := filepath.Join(tmp, "drain-probe")
	if err := p.Go.Build(ctx, "./cmd/drain-probe", binary, "GOOS=linux", "GOARCH="+runtime.GOARCH); err != nil {
		return "", fmt.Errorf("build sondy: %w", err)
	}
	img, err := ociarchive.SingleBinary(binary, ociarchive.BinaryConfig{Path: "/drain-probe", User: ProbeUser, Arch: runtime.GOARCH})
	if err != nil {
		return "", err
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("%s:local-%s", ProbeRepository, strings.TrimPrefix(digest, "sha256:")[:12])
	cli.Step("sonda %s", ref)
	archive := filepath.Join(tmp, "drain-probe.tar")
	if err := img.Write(ref, archive); err != nil {
		return "", err
	}
	if err := p.Importer.Import(ctx, p.Cluster, archive); err != nil {
		return "", fmt.Errorf("import sondy: %w", err)
	}
	return ref, nil
}

// ProbePod zwraca specyfikację sondy. Przestrzeń nazw ma profil Pod Security
// restricted (decyzja 31), więc sonda spełnia go jak każdy inny pod: bez
// tokenu konta usługi, nie jako root, bez eskalacji i uprawnień, z seccomp.
// Zasoby według decyzji 16: requesty i limit pamięci, bez limitu CPU.
func ProbePod(name, namespace, runID, node, image, url string) *corev1.Pod {
	nonRoot := true
	user := int64(ProbeUser)
	noEscalation := false
	readOnly := true
	noToken := false
	grace := int64(1)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{probeNameLabel: probeNameValue, runLabel: runID},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  &noToken,
			NodeSelector:                  map[string]string{"kubernetes.io/hostname": node},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &user,
				RunAsGroup:     &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: image,
				// Obraz jest tylko w węzłach (k3d image import), nie w rejestrze.
				ImagePullPolicy: corev1.PullNever,
				Args:            []string{"-url", url},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnly,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
}
