package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	store "github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// world — stan OpenBao wspólny dla wszystkich podów (Raft).
type world struct {
	initialized bool
	sealed      map[string]bool
	tokens      map[string]bool
	revoked     []string
	loginErr    error
	auths       map[string]string
	audits      map[string]string
	mounts      map[string]string
	policies    map[string]string
	roles       map[string]bool
	voters      int
	// promoteAfter i raftReads — awans nowych replik na głosujące po tylu
	// odczytach konfiguracji Raft (autopilot, czas stabilizacji).
	promoteAfter, raftReads int
	kv                      map[string][]map[string]sensitive.Value
	// loggedIn — role, którymi się logowano.
	loggedIn []string
}

func newWorld() *world {
	return &world{
		sealed: map[string]bool{}, tokens: map[string]bool{}, voters: 3, kv: map[string][]map[string]sensitive.Value{},
		// Audyt z konfiguracji serwera — jest od startu.
		auths: map[string]string{"token": "token"}, audits: map[string]string{"stdout": "file"},
		mounts:   map[string]string{"sys": "system", "cubbyhole": "cubbyhole", "identity": "identity"},
		policies: map[string]string{"default": "...", "root": ""}, roles: map[string]bool{},
	}
}

// server — serwer w podzie z tokenem; operacje administracyjne bez ważnego
// tokenu kończą się 403, jak w OpenBao.
type server struct {
	w     *world
	pod   string
	token string
}

var errDenied = errors.New("HTTP 403 permission denied")

func (s *server) authorized() error {
	if !s.w.tokens[s.token] {
		return errDenied
	}
	return nil
}

func (s *server) SealStatus(context.Context) (store.SealStatus, error) {
	return store.SealStatus{Initialized: s.w.initialized, Sealed: s.w.sealed[s.pod], Type: "static", Version: "2.7.1"}, nil
}
func (s *server) Init(_ context.Context, shares int) (store.Init, error) {
	if s.w.initialized {
		return store.Init{}, errors.New("already initialized")
	}
	s.w.initialized = true
	s.w.tokens["root-1"] = true
	return store.Init{RootToken: sensitive.New("root-1"), RecoveryKeys: []sensitive.Value{sensitive.New("klucz")}}, nil
}
func (s *server) KubernetesLogin(_ context.Context, mount, role string, jwt sensitive.Value) (sensitive.Value, error) {
	if s.w.loginErr != nil {
		return sensitive.Value{}, s.w.loginErr
	}
	if mount != store.AuthMount || jwt.Reveal() != "jwt-"+role || !s.w.roles[role] {
		return sensitive.Value{}, errDenied
	}
	s.w.loggedIn = append(s.w.loggedIn, role)
	token := "tok-" + role
	s.w.tokens[token] = true
	return sensitive.New(token), nil
}
func (s *server) RevokeSelf(context.Context) error {
	if err := s.authorized(); err != nil {
		return err
	}
	delete(s.w.tokens, s.token)
	s.w.revoked = append(s.w.revoked, s.token)
	return nil
}
func (s *server) Audits(context.Context) (map[string]string, error) {
	return clone(s.w.audits), s.authorized()
}
func (s *server) Mounts(context.Context) (map[string]string, error) {
	return clone(s.w.mounts), s.authorized()
}
func (s *server) AuthMethods(context.Context) (map[string]string, error) {
	return clone(s.w.auths), s.authorized()
}
func (s *server) EnableMount(_ context.Context, m store.Mount) error {
	s.w.mounts[m.Path] = m.Type
	return s.authorized()
}
func (s *server) EnableAuth(_ context.Context, a store.Auth) error {
	s.w.auths[a.Path] = a.Type
	return s.authorized()
}
func (s *server) Write(_ context.Context, path string, _ any) error {
	if role, ok := strings.CutPrefix(path, "auth/kubernetes/role/"); ok {
		s.w.roles[role] = true
	}
	return s.authorized()
}
func (s *server) Policies(context.Context) ([]string, error) {
	var names []string
	for n := range s.w.policies {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, s.authorized()
}
func (s *server) Policy(_ context.Context, name string) (string, error) {
	return s.w.policies[name], s.authorized()
}
func (s *server) PutPolicy(_ context.Context, name, hcl string) error {
	s.w.policies[name] = hcl
	return s.authorized()
}
func (s *server) DeletePolicy(_ context.Context, name string) error {
	delete(s.w.policies, name)
	return s.authorized()
}
func (s *server) Roles(context.Context, string) ([]string, error) {
	var names []string
	for n := range s.w.roles {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, s.authorized()
}
func (s *server) DeleteRole(_ context.Context, _, role string) error {
	delete(s.w.roles, role)
	return s.authorized()
}

// RaftServers — trzy repliki; po inicjalizacji głosuje tylko pierwsza,
// a pozostałe autopilot awansuje dopiero po promoteAfter odczytach (czas
// stabilizacji). voters — ile głosuje docelowo.
func (s *server) RaftServers(context.Context) ([]store.RaftServer, error) {
	s.w.raftReads++
	voting := s.w.voters
	if s.w.raftReads <= s.w.promoteAfter {
		voting = 1
	}
	var out []store.RaftServer
	for i := range 3 {
		out = append(out, store.RaftServer{NodeID: fmt.Sprintf("openbao-%d", i), Voter: i < voting, Leader: i == 0})
	}
	return out, s.authorized()
}

func (s *server) KVPut(_ context.Context, mount, path string, data map[string]sensitive.Value) (int, error) {
	if err := s.authorized(); err != nil {
		return 0, err
	}
	s.w.kv[mount+"/"+path] = append(s.w.kv[mount+"/"+path], data)
	return len(s.w.kv[mount+"/"+path]), nil
}
func (s *server) KVVersion(_ context.Context, mount, path string) (int, bool, error) {
	n := len(s.w.kv[mount+"/"+path])
	return n, n > 0, s.authorized()
}

func clone(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// kluster — trzy pody OpenBao; usunięty pod wraca z nowym UID na bieżącej
// rewizji.
type kluster struct {
	pods     []cluster.Pod
	sts      cluster.StatefulSet
	secrets  map[string]map[string][]byte
	nsLabels map[string]string
	deleted  []string
	uid      int
	// volumes — wolumeny danych OpenBao (PVC).
	volumes []string
	// readyAt — od tej chwili niegotowy pod staje się gotowy; listenAt — od
	// tej chwili serwery przyjmują połączenia (wcześniej proces startuje).
	readyAt, listenAt time.Time
}

func newKluster(revisions ...string) *kluster {
	k := &kluster{sts: cluster.StatefulSet{Wanted: 3, Ready: 3, UpdateRevision: "r1"}, secrets: map[string]map[string][]byte{}}
	for i := range 3 {
		rev := "r1"
		if i < len(revisions) {
			rev = revisions[i]
		}
		labels := map[string]string{store.RevisionLabel: rev}
		if i == 0 {
			labels[store.ActiveLabel] = "true"
		}
		k.pods = append(k.pods, cluster.Pod{Name: fmt.Sprintf("openbao-%d", i), Node: fmt.Sprintf("node-%d", i), Labels: labels, Running: true, Ready: true, UID: fmt.Sprintf("uid-%d", i)})
	}
	return k
}

func (k *kluster) EnsureNamespace(_ context.Context, _ string, labels map[string]string) error {
	k.nsLabels = labels
	return nil
}
func (k *kluster) CreateSecretIfMissing(_ context.Context, ns, name string, data map[string][]byte) (bool, error) {
	if _, ok := k.secrets[ns+"/"+name]; ok {
		return false, nil
	}
	cp := map[string][]byte{}
	for key, v := range data {
		cp[key] = slices.Clone(v)
	}
	k.secrets[ns+"/"+name] = cp
	return true, nil
}
func (k *kluster) SecretExists(_ context.Context, ns, name string) (bool, error) {
	_, ok := k.secrets[ns+"/"+name]
	return ok, nil
}
func (k *kluster) PersistentVolumeClaims(context.Context, string, string) ([]string, error) {
	return k.volumes, nil
}
func (k *kluster) Pods(_ context.Context, _, selector string) ([]cluster.Pod, error) {
	// Pod wracający po drainie jest gotowy dopiero po czasie, niezależnie od
	// tego, ile razy ktoś zapyta.
	for i := range k.pods {
		if !k.pods[i].Ready && !k.readyAt.IsZero() && time.Now().After(k.readyAt) {
			k.pods[i].Ready = true
		}
	}
	var out []cluster.Pod
	for _, p := range k.pods {
		if strings.Contains(selector, store.ActiveLabel+"=true") && p.Labels[store.ActiveLabel] != "true" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
func (k *kluster) StatefulSet(context.Context, string, string) (cluster.StatefulSet, error) {
	return k.sts, nil
}
func (k *kluster) DeletePod(_ context.Context, _, name string) error {
	k.deleted = append(k.deleted, name)
	for i, p := range k.pods {
		if p.Name == name {
			k.uid++
			labels := map[string]string{store.RevisionLabel: k.sts.UpdateRevision}
			if p.Labels[store.ActiveLabel] == "true" {
				labels[store.ActiveLabel] = "true"
			}
			k.pods[i] = cluster.Pod{Name: name, Node: p.Node, Labels: labels, Running: true, Ready: true, UID: fmt.Sprintf("nowy-%d", k.uid)}
		}
	}
	return nil
}

// accountRoles — konto w przestrzeni openbao → rola, dla której fałszywy
// serwer przyjmie jego token.
var accountRoles = map[string]string{store.AdminAccount: store.AdminRole, "openbao-dns-writer": "dns-token-writer", store.AppWriterAccount: store.AppWriterRole}

func (k *kluster) ServiceAccountToken(_ context.Context, ns, name, audience string, ttl time.Duration) (sensitive.Value, error) {
	role, ok := accountRoles[name]
	if ns != store.Namespace || !ok || audience != store.Audience || ttl > 10*time.Minute {
		return sensitive.Value{}, errors.New("nie to konto")
	}
	return sensitive.New("jwt-" + role), nil
}

type keys struct{ saved int }

func (k *keys) SaveRecovery(ks []sensitive.Value) (string, error) {
	k.saved += len(ks)
	return "/stan/openbao-recovery.json", nil
}

func newStore(t *testing.T, w *world, k *kluster) (Store, *keys) {
	t.Helper()
	cfg, err := store.LoadConfig(filepath.Join("..", "..", "..", "..", store.Dir))
	if err != nil {
		t.Fatal(err)
	}
	ks := &keys{}
	return Store{
		Cluster: k, Keys: ks, Config: cfg, PSAVersion: "v1.36", Timeout: time.Second,
		// Jak prawdziwy port-forward: do poda, który jeszcze nie nasłuchuje,
		// połączenie się zrywa.
		Dial: func(_ context.Context, pod string, token sensitive.Value) (Server, func(), error) {
			if time.Now().Before(k.listenAt) {
				return nil, nil, errors.New("port-forward: EOF")
			}
			for _, p := range k.pods {
				if p.Name == pod && !p.Ready {
					return nil, nil, errors.New("port-forward: EOF")
				}
			}
			return &server{w: w, pod: pod, token: token.Reveal()}, func() {}, nil
		},
	}, ks
}

// TestBootstrapFresh: pierwsze wdrożenie inicjalizuje, zapisuje klucz
// odzyskiwania, konfiguruje z gita tokenem root i ten token odwołuje.
func TestBootstrapFresh(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, ks := newStore(t, w, k)
	st, err := s.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !w.initialized || ks.saved != 1 {
		t.Errorf("inicjalizacja: %v, kluczy zapisanych %d", w.initialized, ks.saved)
	}
	if w.auths["kubernetes"] != "kubernetes" || w.mounts["docfind"] != "kv" || w.audits["stdout"] != "file" || !w.roles[store.AdminRole] || w.policies[store.AdminRole] == "" {
		t.Errorf("konfiguracja: %+v", w)
	}
	if !slices.Equal(w.revoked, []string{"root-1"}) || w.tokens["root-1"] {
		t.Errorf("token root nie odwołany: %v", w.revoked)
	}
	if len(st.Pods) != 3 || len(st.Raft) != 3 || st.SealType != "static" {
		t.Errorf("stan: %+v", st)
	}
}

// TestBootstrapWaitsForServer: pody są Running, ale serwer jeszcze nie
// słucha — inicjalizacja czeka, aż odpowie (na stagingu: EOF).
func TestBootstrapWaitsForServer(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	k.listenAt = time.Now().Add(time.Second)
	s, _ := newStore(t, w, k)
	s.Timeout = 10 * time.Second
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatalf("inicjalizacja przy startującym serwerze: %v", err)
	}
	if !w.initialized {
		t.Error("OpenBao niezainicjalizowany")
	}
}

// TestBootstrapAgain: kolejne wdrożenie loguje się rolą administracyjną,
// usuwa polityki i role spoza gita, a montowania spoza gita zostawia —
// wyłączenie kasuje dane. Token sesji odwołany.
func TestBootstrapAgain(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.policies["recznie-dodana"] = `path "x" {}`
	w.roles["recznie-dodana"] = true
	w.mounts["secret"] = "kv"
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.policies["recznie-dodana"]; ok || w.roles["recznie-dodana"] {
		t.Errorf("dryf polityk i ról został: %v %v", w.policies, w.roles)
	}
	if w.mounts["secret"] != "kv" {
		t.Error("montowanie spoza gita usunięte — z danymi")
	}
	if !slices.Equal(w.revoked, []string{"root-1", "tok-platform-admin"}) || len(w.tokens) != 0 {
		t.Errorf("tokeny: odwołane %v, ważne %v", w.revoked, w.tokens)
	}
}

// TestBootstrapRefuses: odrzucone logowanie i zmieniony typ montowania to
// warunek niespełniony, bez zmian po cichu.
func TestBootstrapRefuses(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.loginErr = errDenied
	if _, err := s.Bootstrap(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "RUNBOOK") {
		t.Errorf("odrzucone logowanie: %v", err)
	}
	w.loginErr = nil
	delete(w.audits, "stdout")
	if _, err := s.Bootstrap(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "audyt stdout/") {
		t.Errorf("serwer bez audytu: %v", err)
	}
	w.audits["stdout"] = "file"
	w.mounts["docfind"] = "generic"
	if _, err := s.Bootstrap(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "kasuje jego dane") {
		t.Errorf("inny typ montowania: %v", err)
	}
	if len(w.tokens) != 0 {
		t.Errorf("token po błędzie nie odwołany: %v", w.tokens)
	}
}

// TestRollOrder: pody na starszej rewizji wymieniane po jednym, aktywny
// na końcu; pod na bieżącej rewizji zostaje.
func TestRollOrder(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster("r0", "r0", "r1")
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.deleted, []string{"openbao-1", "openbao-0"}) {
		t.Errorf("kolejność wymiany: %v", k.deleted)
	}
}

// TestBootstrapWaitsForPromotion: tuż po inicjalizacji głosuje jedna replika,
// pozostałe autopilot awansuje po czasie stabilizacji — Bootstrap na to
// czeka zamiast zgłaszać brak kworum (tak padło CI na świeżym klastrze).
func TestBootstrapWaitsForPromotion(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	w.promoteAfter = 1
	s, _ := newStore(t, w, k)
	s.Timeout = 10 * time.Second
	st, err := s.Bootstrap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	voters := 0
	for _, r := range st.Raft {
		if r.Voter {
			voters++
		}
	}
	if voters != 3 || w.raftReads <= w.promoteAfter {
		t.Errorf("głosujących %d po %d odczytach", voters, w.raftReads)
	}
}

// TestVerify: zapieczętowany pod albo replika, która nie zostaje
// głosującą — warunek niespełniony.
func TestVerify(t *testing.T) {
	quiet(t)
	for name, mutate := range map[string]func(*world){
		"zapieczętowany": func(w *world) { w.sealed["openbao-2"] = true },
		"bez kworum":     func(w *world) { w.voters = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			w, k := newWorld(), newKluster()
			s, _ := newStore(t, w, k)
			if _, err := s.Bootstrap(context.Background()); err != nil {
				t.Fatal(err)
			}
			mutate(w)
			if _, err := s.Bootstrap(context.Background()); cli.ExitCode(err) != cli.ExitUnmet {
				t.Errorf("kod %d: %v", cli.ExitCode(err), err)
			}
		})
	}
}

// TestPrepare: klucz odpieczętowania 32 bajty, tworzony raz; przestrzeń
// nazw z profilem restricted.
func TestPrepare(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := slices.Clone(k.secrets[store.Namespace+"/"+store.SealSecret][store.SealKey])
	if len(first) != 32 || k.nsLabels["pod-security.kubernetes.io/enforce"] != "restricted" || k.nsLabels["pod-security.kubernetes.io/enforce-version"] != "v1.36" {
		t.Fatalf("klucz %d B, etykiety %v", len(first), k.nsLabels)
	}
	if err := s.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.secrets[store.Namespace+"/"+store.SealSecret][store.SealKey], first) {
		t.Error("klucz odpieczętowania podmieniony")
	}
	// Klucz zniknął, dane zostały — odmowa, nie nowy klucz.
	delete(k.secrets, store.Namespace+"/"+store.SealSecret)
	k.volumes = []string{"data-openbao-0"}
	if err := s.Prepare(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || len(k.secrets) != 0 {
		t.Errorf("nowy klucz przy istniejących danych: %v, %v", err, k.secrets)
	}
}

// TestCheckWaitsForReturningPod: bramka uruchomiona tuż po drainie, gdy pod
// OpenBao dopiero wraca, czeka na jego gotowość zamiast łączyć się z nim
// (w CI: EOF na port-forward).
func TestCheckWaitsForReturningPod(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	s.Timeout = 10 * time.Second
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	k.pods[1].Ready, k.readyAt = false, time.Now().Add(time.Second)
	if _, err := s.Check(context.Background()); err != nil {
		t.Fatalf("bramka przy wracającym podzie: %v", err)
	}
}

// TestCheckRestart: bramka usuwa pod standby (nie aktywny) i czeka, aż wróci
// gotowy pod nowym UID.
func TestCheckRestart(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RestartedPod == "" || rep.RestartedPod == "openbao-0" || !slices.Equal(k.deleted, []string{rep.RestartedPod}) {
		t.Errorf("restart: %q, usunięte %v", rep.RestartedPod, k.deleted)
	}
}

// TestPutSecret: zapis rolą wąską (nie administracyjną), wersja z metadanych,
// token odwołany po każdej operacji.
func TestPutSecret(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.loggedIn, w.revoked = nil, nil
	ctx := context.Background()
	if _, ok, err := s.SecretVersion(ctx, "openbao-dns-writer", "dns-token-writer", "platform/cloudflare"); err != nil || ok {
		t.Fatalf("pusty sekret: %v, %v", ok, err)
	}
	v, err := s.PutSecret(ctx, "openbao-dns-writer", "dns-token-writer", "platform/cloudflare", map[string]sensitive.Value{"api-token": sensitive.New("cf")})
	if err != nil || v != 1 || w.kv["docfind/platform/cloudflare"][0]["api-token"].Reveal() != "cf" {
		t.Fatalf("zapis: wersja %d, %v", v, err)
	}
	if !slices.Equal(w.loggedIn, []string{"dns-token-writer", "dns-token-writer"}) || len(w.revoked) != 2 || len(w.tokens) != 0 {
		t.Errorf("role %v, odwołane %v, ważne %v", w.loggedIn, w.revoked, w.tokens)
	}
}

// TestEnsureAppSecrets: token metryk generowany raz — kolejne wdrożenie go
// nie zmienia (zmiana to rotacja, nie skutek uboczny wdrożenia).
func TestEnsureAppSecrets(t *testing.T) {
	quiet(t)
	w, k := newWorld(), newKluster()
	s, _ := newStore(t, w, k)
	if _, err := s.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.EnsureAppSecrets(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	versions := w.kv[store.KVMount+"/"+store.AppMetricsPath]
	if len(versions) != 1 || len(versions[0][store.AppMetricsField].Reveal()) != 43 {
		t.Errorf("wersje tokenu metryk: %d", len(versions))
	}
}
