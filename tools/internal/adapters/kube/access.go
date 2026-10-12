package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	//lint:ignore SA1019 portforward z client-go 0.36.5 przyjmuje i zwraca typy z tego pakietu, nie z k8s.io/streaming — do zmiany razem z client-go.
	"k8s.io/apimachinery/pkg/util/httpstream"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// CreateSecretIfMissing zakłada niezmienny (immutable) Secret, jeśli go nie
// ma; istniejącego nie dotyka i mówi, czy coś utworzył. Dla danych, których
// podmiana jest utratą, a nie zmianą — klucz odpieczętowania OpenBao:
// z innym kluczem dane w Raft są nie do odczytania. Niezmienność pilnuje
// tego też po stronie API: aktualizacja danych jest odrzucana.
func (c *Cluster) CreateSecretIfMissing(ctx context.Context, namespace, name string, data map[string][]byte) (bool, error) {
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
		Immutable:  &immutable,
	}
	_, err := c.core.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("zapis Secretu %s/%s: %w", namespace, name, err)
	}
	return true, nil
}

// PersistentVolumeClaims zwraca nazwy wolumenów przestrzeni nazw pasujących
// do selektora etykiet.
func (c *Cluster) PersistentVolumeClaims(ctx context.Context, namespace, selector string) ([]string, error) {
	list, err := c.core.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, pvc := range list.Items {
		names = append(names, pvc.Name)
	}
	return names, nil
}

// ServiceAccountToken wydaje krótkotrwały token konta (TokenRequest) dla
// jednego odbiorcy. Token nie trafia do żadnego Secreta i wygasa sam —
// w przeciwieństwie do tokenów z Secretów typu service-account-token.
// Kubernetes nie wydaje tokenów krótszych niż 10 minut.
func (c *Cluster) ServiceAccountToken(ctx context.Context, namespace, name, audience string, ttl time.Duration) (sensitive.Value, error) {
	seconds := int64(ttl.Seconds())
	tr, err := c.core.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &seconds},
	}, metav1.CreateOptions{})
	if err != nil {
		return sensitive.Value{}, fmt.Errorf("token konta %s/%s: %w", namespace, name, err)
	}
	return sensitive.New(tr.Status.Token), nil
}

// StatefulSet zwraca stan StatefulSetu.
func (c *Cluster) StatefulSet(ctx context.Context, namespace, name string) (cluster.StatefulSet, error) {
	s, err := c.core.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return cluster.StatefulSet{}, err
	}
	wanted := int32(1)
	if s.Spec.Replicas != nil {
		wanted = *s.Spec.Replicas
	}
	return cluster.StatefulSet{Wanted: wanted, Ready: s.Status.ReadyReplicas, UpdateRevision: s.Status.UpdateRevision}, nil
}

// PortForward przekierowuje losowy port na 127.0.0.1 na port poda i zwraca
// adres host:port. Tylko pętla zwrotna, nigdy 0.0.0.0. Połączenie idzie przez
// API server i kubelet do przestrzeni sieciowej poda, więc TLS usługi
// w podzie zostaje od końca do końca — w przeciwieństwie do proxy API
// servera, które nie sprawdza certyfikatu poda. stop zamyka przekierowanie;
// koniec ctx też.
//
// Najpierw WebSocket, potem SPDY — w tej kolejności, co kubectl: SPDY jest
// w Kubernetesie wycofywany.
func (c *Cluster) PortForward(ctx context.Context, namespace, pod string, port int) (string, func(), error) {
	if c.config == nil {
		return "", nil, errors.New("port-forward wymaga klastra połączonego przez Connect")
	}
	url := c.core.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(c.config)
	if err != nil {
		return "", nil, err
	}
	var dialer httpstream.Dialer = spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	ws, err := portforward.NewSPDYOverWebsocketDialer(url, c.config)
	if err != nil {
		return "", nil, err
	}
	dialer = portforward.NewFallbackDialer(ws, dialer, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})

	stopCh, readyCh := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopCh) }) }
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stopCh, readyCh, io.Discard, cli.Err)
	if err != nil {
		return "", nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-errCh:
		return "", nil, fmt.Errorf("port-forward %s/%s:%d: %w", namespace, pod, port, err)
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		stop()
		return "", nil, fmt.Errorf("port-forward %s/%s:%d: brak portu lokalnego (%v)", namespace, pod, port, err)
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-stopCh:
		}
	}()
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), stop, nil
}
