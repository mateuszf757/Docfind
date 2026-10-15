package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/httpclient"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/openbao"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	store "github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
	"github.com/mateuszf757/Docfind/tools/internal/stages/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/stages/rotation"
	"github.com/mateuszf757/Docfind/tools/internal/stages/secrets"
)

// secretStore składa magazyn sekretów (stages/secrets) na klastrze t.
func (w *workspace) secretStore(def environment.Definition, t cluster.Target) (secrets.Store, error) {
	c, err := kube.Connect(t, w.runner)
	if err != nil {
		return secrets.Store{}, err
	}
	cfg, err := store.LoadConfig(filepath.Join(w.root, store.Dir))
	if err != nil {
		return secrets.Store{}, err
	}
	p, err := pins.Load(w.root)
	if err != nil {
		return secrets.Store{}, err
	}
	psa, err := cluster.PSAVersion(p.Get(pins.KubernetesVersion))
	if err != nil {
		return secrets.Store{}, err
	}
	return secrets.Store{
		Cluster: c, Config: cfg, PSAVersion: psa,
		Keys: recoveryKeys{dir: stateDir(def.Cluster.Name), cluster: def.Cluster.Name},
		Dial: dialOpenBao(c),
	}, nil
}

func (w *workspace) secretStores(def environment.Definition) func(cluster.Target) (deploy.SecretStore, error) {
	return func(t cluster.Target) (deploy.SecretStore, error) {
		return w.secretStore(def, t)
	}
}

// dialOpenBao łączy się z serwerem w podzie przez port-forward na
// 127.0.0.1, po TLS sprawdzanym CA z Secreta certyfikatu OpenBao.
func dialOpenBao(c *kube.Cluster) secrets.Dial {
	return func(ctx context.Context, pod string, token sensitive.Value) (secrets.Server, func(), error) {
		data, err := c.SecretData(ctx, store.Namespace, store.TLSSecret)
		if err != nil {
			return nil, nil, fmt.Errorf("certyfikat OpenBao %s/%s: %w", store.Namespace, store.TLSSecret, err)
		}
		addr, stop, err := c.PortForward(ctx, store.Namespace, pod, store.Port)
		if err != nil {
			return nil, nil, err
		}
		client, err := openbao.New(addr, data["ca.crt"], store.ServerName)
		if err != nil {
			stop()
			return nil, nil, err
		}
		return client.WithToken(token), stop, nil
	}
}

// stateDir — stan klastra na hoście, poza repozytorium:
// $XDG_STATE_HOME/docfind/<klaster> (domyślnie ~/.local/state). Nie .cache/
// w repozytorium: katalog roboczy bywa kopiowany, pakowany i udostępniany.
func stateDir(clusterName string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "docfind", clusterName)
}

// recoveryKeys zapisuje klucze odzyskiwania OpenBao do pliku 0600
// w katalogu 0700 — jedyne miejsce, w którym ich treść opuszcza pamięć.
type recoveryKeys struct{ dir, cluster string }

const recoveryFile = "openbao-recovery.json"

func (r recoveryKeys) SaveRecovery(keys []sensitive.Value) (string, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(r.dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(r.dir, recoveryFile)
	// Plik z poprzedniego klastra o tej nazwie (usuniętego bez `cluster
	// down`) — nie nadpisujemy, tylko odsuwamy: ktoś mógł go jeszcze
	// potrzebować.
	if _, err := os.Stat(path); err == nil {
		old := fmt.Sprintf("%s.%s.old", path, time.Now().UTC().Format("20060102T150405Z"))
		if err := os.Rename(path, old); err != nil {
			return "", err
		}
		cli.Warn("poprzedni klucz odzyskiwania przeniesiony do %s — klaster %s był już inicjalizowany", old, r.cluster)
	}
	doc := struct {
		Cluster      string    `json:"cluster"`
		Created      time.Time `json:"created"`
		RecoveryKeys []string  `json:"recovery_keys_base64"`
	}{Cluster: r.cluster, Created: time.Now().UTC()}
	for _, k := range keys {
		doc.RecoveryKeys = append(doc.RecoveryKeys, k.Reveal())
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return "", errors.Join(err, f.Close())
	}
	// Zapis na dysk przed odwołaniem tokenu root: po nim klucz jest jedyną
	// drogą awaryjną.
	if err := f.Sync(); err != nil {
		return "", errors.Join(err, f.Close())
	}
	return path, f.Close()
}

// runCheckOpenBao — bramka magazynu sekretów: stan replik, Raft i restart
// standby bez człowieka. Raport w .cache/reports/openbao.json.
func runCheckOpenBao(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check openbao"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	if err := def.RequireCluster(); err != nil {
		return err
	}
	s, err := w.secretStore(def, def.Target(w.root))
	if err != nil {
		return err
	}
	rep, err := s.Check(ctx)
	return w.withReport("openbao", rep, err)
}

// runCheckRotation — warunek zakończenia Etapu 4: rotacja tokenu metryk
// dociera do każdego poda API bez restartu (stages/rotation). Raport
// w .cache/reports/rotation-<środowisko>.json, bez wartości tokenów.
func runCheckRotation(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check rotation"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	if err := def.RequireCluster(); err != nil {
		return err
	}
	if def.Cluster.Provider != environment.ProviderK3d {
		return cli.Unmet("środowisko %s nie ma platformy z OpenBao i ESO — rotację sprawdza się na klastrach k3d", def.Name)
	}
	t := def.Target(w.root)
	c, err := kube.Connect(t, w.runner)
	if err != nil {
		return err
	}
	s, err := w.secretStore(def, t)
	if err != nil {
		return err
	}
	gate := rotation.Gate{
		Cluster: c, Vault: s, HTTP: podProber{},
		Namespace: def.App.Namespace,
		Selector:  fmt.Sprintf("app.kubernetes.io/name=docfind,app.kubernetes.io/instance=%s,app.kubernetes.io/component=api", def.App.Release),
		Secret:    environment.AppSecret, Key: environment.MetricsTokenSecret,
		// Port API w podzie (api.config.service.port w charcie).
		Port: 8000,
		// refreshInterval ExternalSecret (1 min) + podmiana plików przez
		// kubelet (okres synchronizacji poda, 1 min) z zapasem.
		Budget: contract.RotationPhaseBudget, Interval: 2 * time.Second,
		NewToken: secrets.NewToken,
	}
	rep, err := gate.Run(ctx)
	return w.withReport("rotation-"+def.Name, rep, err)
}

// podProber — kod odpowiedzi poda przez port-forward na 127.0.0.1.
type podProber struct{}

func (podProber) Status(ctx context.Context, addr, path string, token sensitive.Value) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, err
	}
	resp, err := httpclient.Client{}.Get(ctx, loopback.Request{Port: port, Path: path, Timeout: 3 * time.Second, Bearer: token})
	return resp.Status, err
}
