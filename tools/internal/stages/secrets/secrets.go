// Package secrets stawia i utrzymuje magazyn sekretów platformy — OpenBao
// (decyzja 42): przygotowanie przed instalacją charta (przestrzeń nazw,
// klucz odpieczętowania), inicjalizacja przy pierwszym wdrożeniu, wymiana
// podów po zmianie szablonu, konfiguracja z gita i bramka stanu.
//
// Żadna wartość poufna nie przechodzi przez wyjście: klucz odpieczętowania
// powstaje i zostaje w klastrze, klucz odzyskiwania idzie do pliku poza
// repozytorium, a tokeny — root po inicjalizacji i administracyjny przy
// każdym kolejnym wdrożeniu — żyją w pamięci i są odwoływane po pracy.
package secrets

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	store "github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Cluster — klaster (adapters/kube).
type Cluster interface {
	EnsureNamespace(ctx context.Context, name string, labels map[string]string) error
	CreateSecretIfMissing(ctx context.Context, namespace, name string, data map[string][]byte) (bool, error)
	SecretExists(ctx context.Context, namespace, name string) (bool, error)
	PersistentVolumeClaims(ctx context.Context, namespace, selector string) ([]string, error)
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	StatefulSet(ctx context.Context, namespace, name string) (cluster.StatefulSet, error)
	DeletePod(ctx context.Context, namespace, pod string) error
	ServiceAccountToken(ctx context.Context, namespace, name, audience string, ttl time.Duration) (sensitive.Value, error)
}

// Server — API jednego serwera OpenBao (adapters/openbao).
type Server interface {
	SealStatus(ctx context.Context) (store.SealStatus, error)
	Init(ctx context.Context, shares int) (store.Init, error)
	KubernetesLogin(ctx context.Context, mount, role string, jwt sensitive.Value) (sensitive.Value, error)
	RevokeSelf(ctx context.Context) error
	Audits(ctx context.Context) (map[string]string, error)
	Mounts(ctx context.Context) (map[string]string, error)
	AuthMethods(ctx context.Context) (map[string]string, error)
	EnableMount(ctx context.Context, m store.Mount) error
	EnableAuth(ctx context.Context, a store.Auth) error
	Write(ctx context.Context, path string, data any) error
	Policies(ctx context.Context) ([]string, error)
	Policy(ctx context.Context, name string) (string, error)
	PutPolicy(ctx context.Context, name, hcl string) error
	DeletePolicy(ctx context.Context, name string) error
	Roles(ctx context.Context, mount string) ([]string, error)
	DeleteRole(ctx context.Context, mount, role string) error
	RaftServers(ctx context.Context) ([]store.RaftServer, error)
	KVPut(ctx context.Context, mount, path string, data map[string]sensitive.Value) (int, error)
	KVVersion(ctx context.Context, mount, path string) (int, bool, error)
}

// Dial łączy się z serwerem w podzie (port-forward, TLS z CA platformy),
// z tokenem albo bez; close zamyka połączenie.
type Dial func(ctx context.Context, pod string, token sensitive.Value) (srv Server, close func(), err error)

// Keys — klucze odzyskiwania poza repozytorium i klastrem.
type Keys interface {
	SaveRecovery(keys []sensitive.Value) (path string, err error)
}

// Store — OpenBao na klastrze.
type Store struct {
	Cluster Cluster
	Dial    Dial
	Keys    Keys
	Config  store.Config
	// PSAVersion — wersja profilu Pod Security restricted (decyzja 31).
	PSAVersion string
	// Timeout — na każde czekanie; zero — 5 minut (pobranie obrazu przy
	// pierwszym wdrożeniu).
	Timeout time.Duration
}

const selector = "app.kubernetes.io/name=openbao,component=server"

// adminTokenTTL — token konta openbao-admin dla logowania; Kubernetes nie
// wydaje krótszych.
const adminTokenTTL = 10 * time.Minute

func (s Store) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 5 * time.Minute
}

// Prepare zakłada to, czego chart potrzebuje przed startem podów:
// przestrzeń nazw z profilem restricted i klucz odpieczętowania. Klucz
// powstaje raz i nigdy nie jest podmieniany — z innym kluczem dane w Raft
// są nie do odczytania.
func (s Store) Prepare(ctx context.Context) error {
	if err := s.Cluster.EnsureNamespace(ctx, store.Namespace, map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": s.PSAVersion,
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    s.PSAVersion,
	}); err != nil {
		return err
	}
	// Klucz zniknął, a dane zostały: nowy klucz ich nie otworzy, a pody
	// zapieczętowane „niezgodnym kluczem" zgubiłyby przyczynę. Odmowa
	// zamiast generowania.
	exists, err := s.Cluster.SecretExists(ctx, store.Namespace, store.SealSecret)
	if err != nil {
		return err
	}
	if !exists {
		volumes, err := s.Cluster.PersistentVolumeClaims(ctx, store.Namespace, selector)
		if err != nil {
			return err
		}
		if len(volumes) > 0 {
			return cli.Unmet("brak klucza odpieczętowania %s/%s przy danych OpenBao (%s) — nowy klucz ich nie otworzy; docs/RUNBOOK.md, OpenBao: utrata klucza odpieczętowania", store.Namespace, store.SealSecret, strings.Join(volumes, ", "))
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	created, err := s.Cluster.CreateSecretIfMissing(ctx, store.Namespace, store.SealSecret, map[string][]byte{store.SealKey: key})
	clear(key)
	if err != nil {
		return err
	}
	if created {
		cli.Step("OpenBao: klucz odpieczętowania wygenerowany (Secret %s/%s, niezmienny)", store.Namespace, store.SealSecret)
	} else {
		cli.Step("OpenBao: klucz odpieczętowania jest — bez zmian")
	}
	return nil
}

// Status — stan OpenBao po wdrożeniu (dowód w raporcie).
type Status struct {
	Version  string             `json:"version"`
	SealType string             `json:"seal_type"`
	Pods     []PodStatus        `json:"pods"`
	Raft     []store.RaftServer `json:"raft"`
}

// PodStatus — jeden serwer.
type PodStatus struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	Active bool   `json:"active"`
	Sealed bool   `json:"sealed"`
}

// Bootstrap doprowadza zainstalowany chart do działającego magazynu:
// inicjalizacja (tylko pierwszy raz), wymiana podów na starszej rewizji,
// konfiguracja z gita, sprawdzenie Raft i pieczęci każdego poda.
func (s Store) Bootstrap(ctx context.Context) (Status, error) {
	if err := s.wait(ctx, "pody OpenBao uruchomione", s.allRunning); err != nil {
		return Status{}, err
	}
	token, fresh, err := s.initialize(ctx)
	if err != nil {
		return Status{}, err
	}
	if err := s.wait(ctx, "pody OpenBao odpieczętowane i w Raft", s.allReady); err != nil {
		return Status{}, err
	}
	if err := s.roll(ctx); err != nil {
		return Status{}, err
	}
	var status Status
	err = s.session(ctx, token, fresh, func(srv Server) error {
		if err := s.apply(ctx, srv); err != nil {
			return err
		}
		status, err = s.verify(ctx, srv)
		return err
	})
	return status, err
}

// initialize inicjalizuje OpenBao przez pierwszy pod; pozostałe dołączają
// do niego przez retry_join. Zwraca token root tylko wtedy, gdy właśnie
// zainicjalizował (fresh).
func (s Store) initialize(ctx context.Context) (token sensitive.Value, fresh bool, err error) {
	err = s.withServer(ctx, store.StatefulSet+"-0", sensitive.Value{}, func(srv Server) error {
		st, err := srv.SealStatus(ctx)
		if err != nil {
			return err
		}
		if st.Initialized {
			return nil
		}
		cli.Step("OpenBao %s: inicjalizacja (pieczęć %s, jeden klucz odzyskiwania)", st.Version, st.Type)
		init, err := srv.Init(ctx, 1)
		if err != nil {
			return err
		}
		path, err := s.Keys.SaveRecovery(init.RecoveryKeys)
		if err != nil {
			return fmt.Errorf("klucz odzyskiwania nie zapisany: %w — OpenBao jest zainicjalizowany bez drogi awaryjnej, odtwórz klaster", err)
		}
		cli.Step("OpenBao: klucz odzyskiwania w %s (0600, poza repozytorium) — przenieś do menedżera haseł", path)
		token, fresh = init.RootToken, true
		return nil
	})
	return token, fresh, err
}

// session wykonuje work tokenem root (po inicjalizacji) albo tokenem roli
// administracyjnej i zawsze odwołuje token po pracy — także po błędzie.
func (s Store) session(ctx context.Context, token sensitive.Value, fresh bool, work func(Server) error) error {
	active, err := s.activePod(ctx)
	if err != nil {
		return err
	}
	if !fresh {
		if token, err = s.login(ctx, active, store.AdminAccount, store.AdminRole); err != nil {
			return err
		}
	}
	return s.withServer(ctx, active, token, func(srv Server) (err error) {
		defer func() {
			// Bez anulowania: odwołanie ma się odbyć także po przerwaniu pracy.
			if revokeErr := srv.RevokeSelf(context.WithoutCancel(ctx)); revokeErr != nil {
				err = errors.Join(err, fmt.Errorf("odwołanie tokenu OpenBao: %w", revokeErr))
			} else if fresh {
				cli.Step("OpenBao: token root odwołany")
			}
		}()
		return work(srv)
	})
}

// login wymienia token konta account z przestrzeni openbao (TokenRequest,
// 10 minut, dla odbiorcy openbao) na token roli role.
func (s Store) login(ctx context.Context, pod, account, role string) (sensitive.Value, error) {
	jwt, err := s.Cluster.ServiceAccountToken(ctx, store.Namespace, account, store.Audience, adminTokenTTL)
	if err != nil {
		return sensitive.Value{}, err
	}
	var token sensitive.Value
	err = s.withServer(ctx, pod, sensitive.Value{}, func(srv Server) error {
		t, err := srv.KubernetesLogin(ctx, store.AuthMount, role, jwt)
		if err != nil {
			return cli.Unmet("logowanie rolą %s nie przeszło (%v) — dostęp awaryjny kluczem odzyskiwania: docs/RUNBOOK.md, OpenBao", role, err)
		}
		token = t
		return nil
	})
	return token, err
}

// asRole wykonuje work tokenem wąskiej roli (nie administracyjnej) i odwołuje
// go po pracy — także po błędzie.
func (s Store) asRole(ctx context.Context, account, role string, work func(Server) error) error {
	active, err := s.activePod(ctx)
	if err != nil {
		return err
	}
	token, err := s.login(ctx, active, account, role)
	if err != nil {
		return err
	}
	return s.withServer(ctx, active, token, func(srv Server) (err error) {
		defer func() {
			if revokeErr := srv.RevokeSelf(context.WithoutCancel(ctx)); revokeErr != nil {
				err = errors.Join(err, fmt.Errorf("odwołanie tokenu OpenBao: %w", revokeErr))
			}
		}()
		return work(srv)
	})
}

// PutSecret zapisuje nową wersję sekretu path (KV v2) rolą role konta
// account i zwraca jej numer.
func (s Store) PutSecret(ctx context.Context, account, role, path string, data map[string]sensitive.Value) (int, error) {
	var version int
	err := s.asRole(ctx, account, role, func(srv Server) (err error) {
		version, err = srv.KVPut(ctx, store.KVMount, path, data)
		return err
	})
	return version, err
}

// SecretVersion zwraca bieżącą wersję sekretu path (z metadanych, bez
// wartości) rolą role konta account.
func (s Store) SecretVersion(ctx context.Context, account, role, path string) (version int, exists bool, err error) {
	err = s.asRole(ctx, account, role, func(srv Server) (err error) {
		version, exists, err = srv.KVVersion(ctx, store.KVMount, path)
		return err
	})
	return version, exists, err
}

// roll wymienia pody na starszej rewizji szablonu (OnDelete): najpierw
// standby, aktywny na końcu. Aktywny przy SIGTERM oddaje przywództwo; gdyby
// szedł pierwszy, przywództwo trafiałoby na pod, który zaraz też wypadnie.
// Każdy następny dopiero, gdy poprzedni wrócił odpieczętowany — kworum Raft
// nie spada poniżej dwóch.
func (s Store) roll(ctx context.Context) error {
	sts, err := s.Cluster.StatefulSet(ctx, store.Namespace, store.StatefulSet)
	if err != nil {
		return err
	}
	pods, err := s.Cluster.Pods(ctx, store.Namespace, selector)
	if err != nil {
		return err
	}
	var outdated []cluster.Pod
	for _, p := range pods {
		if p.Labels[store.RevisionLabel] != sts.UpdateRevision {
			outdated = append(outdated, p)
		}
	}
	slices.SortStableFunc(outdated, func(a, b cluster.Pod) int {
		return cmp.Compare(boolInt(a.Labels[store.ActiveLabel] == "true"), boolInt(b.Labels[store.ActiveLabel] == "true"))
	})
	for _, p := range outdated {
		cli.Step("OpenBao: wymiana %s na rewizję %s", p.Name, sts.UpdateRevision)
		if err := s.Cluster.DeletePod(ctx, store.Namespace, p.Name); err != nil {
			return err
		}
		if err := s.wait(ctx, "pod "+p.Name+" na nowej rewizji i gotowy", func(ctx context.Context) (bool, error) {
			q, ok, err := s.pod(ctx, p.Name)
			return ok && q.UID != p.UID && q.Labels[store.RevisionLabel] == sts.UpdateRevision && q.Ready, err
		}); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// apply doprowadza serwer do konfiguracji z gita. Metody uwierzytelniania,
// polityki i role najpierw: po inicjalizacji token root żyje tylko do końca
// tej sesji, a rola administracyjna jest drogą do każdego kolejnego
// wdrożenia.
func (s Store) apply(ctx context.Context, srv Server) error {
	c := s.Config
	auths, err := srv.AuthMethods(ctx)
	if err != nil {
		return err
	}
	for _, a := range c.Auth {
		switch t, ok := auths[a.Path]; {
		case !ok:
			if err := srv.EnableAuth(ctx, a); err != nil {
				return err
			}
			cli.Step("OpenBao: metoda %s/ (%s) włączona", a.Path, a.Type)
		case t != a.Type:
			return cli.Unmet("metoda %s/ jest typu %s, a w gicie %s — zmiana typu to wyłączenie metody razem z jej rolami; tylko ręcznie i świadomie", a.Path, t, a.Type)
		}
		if len(a.Config) > 0 {
			if err := srv.Write(ctx, "auth/"+a.Path+"/config", a.Config); err != nil {
				return err
			}
		}
		delete(auths, a.Path)
	}
	for _, p := range slices.Sorted(maps.Keys(auths)) {
		if p != "token" {
			cli.Warn("OpenBao: metoda %s/ spoza gita — zostawiona; usuń ją albo dopisz do %s", p, store.ConfigFile)
		}
	}

	existing, err := srv.Policies(ctx)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(c.Policies)) {
		current, err := srv.Policy(ctx, name)
		if err != nil {
			return err
		}
		if current == c.Policies[name] {
			continue
		}
		if err := srv.PutPolicy(ctx, name, c.Policies[name]); err != nil {
			return err
		}
		cli.Step("OpenBao: polityka %s zapisana", name)
	}
	for _, name := range existing {
		if _, ok := c.Policies[name]; ok || name == "root" || name == "default" {
			continue
		}
		if err := srv.DeletePolicy(ctx, name); err != nil {
			return err
		}
		cli.Step("OpenBao: polityka %s spoza gita usunięta", name)
	}

	wanted := map[string]map[string]bool{}
	for _, r := range c.Roles {
		if err := srv.Write(ctx, "auth/"+r.Auth+"/role/"+r.Name, r.Body()); err != nil {
			return err
		}
		if wanted[r.Auth] == nil {
			wanted[r.Auth] = map[string]bool{}
		}
		wanted[r.Auth][r.Name] = true
	}
	for _, a := range c.Auth {
		if a.Type != "kubernetes" {
			continue
		}
		roles, err := srv.Roles(ctx, a.Path)
		if err != nil {
			return err
		}
		for _, r := range roles {
			if wanted[a.Path][r] {
				continue
			}
			if err := srv.DeleteRole(ctx, a.Path, r); err != nil {
				return err
			}
			cli.Step("OpenBao: rola %s/%s spoza gita usunięta", a.Path, r)
		}
	}
	cli.Step("OpenBao: %d polityk, %d ról zgodnych z gitem", len(c.Policies), len(c.Roles))

	audits, err := srv.Audits(ctx)
	if err != nil {
		return err
	}
	// Audyt deklaruje konfiguracja serwera (OpenBao 2.7 nie włącza go przez
	// API) — tutaj tylko sprawdzenie, że działa.
	for _, a := range c.Audit {
		if t, ok := audits[a.Path]; !ok || t != a.Type {
			return cli.Unmet("audyt %s/ (%s) zadeklarowany w %s/values.yaml, a serwer go nie ma (jest: %q) — konfiguracja serwera rozjechała się z gitem", a.Path, a.Type, store.Dir, t)
		}
		delete(audits, a.Path)
	}
	for _, p := range slices.Sorted(maps.Keys(audits)) {
		cli.Warn("OpenBao: audyt %s/ spoza gita — zostawiony", p)
	}

	mounts, err := srv.Mounts(ctx)
	if err != nil {
		return err
	}
	for _, m := range c.Mounts {
		switch t, ok := mounts[m.Path]; {
		case !ok:
			if err := srv.EnableMount(ctx, m); err != nil {
				return err
			}
			cli.Step("OpenBao: montowanie %s/ (%s) włączone", m.Path, m.Type)
		case t != m.Type:
			return cli.Unmet("montowanie %s/ jest typu %s, a w gicie %s — wyłączenie montowania kasuje jego dane", m.Path, t, m.Type)
		}
		delete(mounts, m.Path)
	}
	for _, p := range slices.Sorted(maps.Keys(mounts)) {
		if !builtinMounts[p] {
			cli.Warn("OpenBao: montowanie %s/ spoza gita — zostawione (wyłączenie kasuje dane)", p)
		}
	}
	return nil
}

// builtinMounts — montowania, które OpenBao ma zawsze.
var builtinMounts = map[string]bool{"sys": true, "cubbyhole": true, "identity": true}

// verify sprawdza, że każda replika głosuje w Raft, jest jeden lider i żaden
// pod nie jest zapieczętowany.
func (s Store) verify(ctx context.Context, srv Server) (Status, error) {
	var st Status
	sts, err := s.Cluster.StatefulSet(ctx, store.Namespace, store.StatefulSet)
	if err != nil {
		return st, err
	}
	// Autopilot dołącza nową replikę jako niegłosującą i awansuje ją po
	// czasie stabilizacji (server_stabilization_time, domyślnie 10 s). Tuż
	// po inicjalizacji albo po odtworzeniu poda z pustym wolumenem nie
	// wszystkie głosują — to stan przejściowy, więc czekamy na stan
	// docelowy, a nie oceniamy jednej chwili (w CI sprawdzenie przyszło
	// sekundy po inicjalizacji: 1 głosujący z 3).
	voters, leaders := 0, 0
	raftErr := poll.Until(ctx, s.timeout(), 2*time.Second, func(ctx context.Context) (bool, error) {
		servers, err := srv.RaftServers(ctx)
		if err != nil {
			return false, err
		}
		st.Raft, voters, leaders = servers, 0, 0
		for _, r := range servers {
			if r.Voter {
				voters++
			}
			if r.Leader {
				leaders++
			}
		}
		return voters == int(sts.Wanted) && leaders == 1, nil
	})
	if raftErr != nil && !errors.Is(raftErr, poll.ErrTimeout) {
		return st, raftErr
	}
	pods, err := s.Cluster.Pods(ctx, store.Namespace, selector)
	if err != nil {
		return st, err
	}
	v := &cli.Verdict{}
	for _, p := range pods {
		err := s.withServer(ctx, p.Name, sensitive.Value{}, func(srv Server) error {
			seal, err := srv.SealStatus(ctx)
			if err != nil {
				return err
			}
			st.Version, st.SealType = seal.Version, seal.Type
			st.Pods = append(st.Pods, PodStatus{Name: p.Name, Node: p.Node, Active: p.Labels[store.ActiveLabel] == "true", Sealed: seal.Sealed || !seal.Initialized})
			if seal.Sealed || !seal.Initialized {
				v.Fail("%s: zapieczętowany albo niezainicjalizowany", p.Name)
			}
			return nil
		})
		if err != nil {
			return st, err
		}
	}
	if voters != int(sts.Wanted) || leaders != 1 {
		v.Fail("Raft: po %v %d głosujących z %d, liderów %d", s.timeout(), voters, sts.Wanted, leaders)
	}
	if err := v.Err("OpenBao"); err != nil {
		return st, err
	}
	cli.Step("OpenBao %s: %d replik odpieczętowanych (pieczęć %s), Raft %d/%d głosujących", st.Version, len(st.Pods), st.SealType, voters, sts.Wanted)
	return st, nil
}

// Report — wynik `dft check openbao`.
type Report struct {
	Status
	// RestartedPod i RestartSeconds — pod standby usunięty przez bramkę i czas
	// do jego powrotu odpieczętowanego, bez człowieka.
	RestartedPod   string  `json:"restarted_pod"`
	RestartSeconds float64 `json:"restart_seconds"`
}

// Check to bramka magazynu: każda replika odpieczętowana i głosująca,
// a usunięty pod standby wraca sam — odpieczętowany kluczem z Secreta
// i z powrotem w Raft. Ręczny unseal po restarcie zatrzymałby drain
// kolejnego węzła na PDB (decyzja 42).
func (s Store) Check(ctx context.Context) (Report, error) {
	var rep Report
	// Poprzedni krok mógł właśnie odtworzyć pod OpenBao: wolumen local-path
	// trzyma go na węźle, więc po drainie wraca dopiero po uncordon. W CI
	// bramka przyszła 0,2 s po drainie, gdy openbao-1 dopiero startował,
	// i port-forward skończył się EOF. Ocena dotyczy stanu ustalonego, nie
	// powrotu poda — najpierw gotowość wszystkich replik.
	if err := s.wait(ctx, "pody OpenBao gotowe", s.allReady); err != nil {
		return rep, err
	}
	err := s.session(ctx, sensitive.Value{}, false, func(srv Server) error {
		st, err := s.verify(ctx, srv)
		rep.Status = st
		if err != nil {
			return err
		}
		var victim string
		for _, p := range st.Pods {
			if !p.Active {
				victim = p.Name
				break
			}
		}
		if victim == "" {
			return cli.Unmet("OpenBao: brak poda standby do sprawdzenia restartu")
		}
		old, ok, err := s.pod(ctx, victim)
		if err != nil {
			return err
		}
		if !ok {
			return cli.Unmet("pod %s zniknął przed restartem — coś innego wymienia pody OpenBao w trakcie bramki", victim)
		}
		cli.Step("OpenBao: restart %s (standby) — ma wrócić odpieczętowany bez człowieka", victim)
		started := time.Now()
		if err := s.Cluster.DeletePod(ctx, store.Namespace, victim); err != nil {
			return err
		}
		if err := s.wait(ctx, "pod "+victim+" odpieczętowany po restarcie", func(ctx context.Context) (bool, error) {
			q, ok, err := s.pod(ctx, victim)
			return ok && q.UID != old.UID && q.Ready, err
		}); err != nil {
			return err
		}
		rep.RestartedPod, rep.RestartSeconds = victim, time.Since(started).Seconds()
		cli.Step("OpenBao: %s odpieczętowany po %.1f s", victim, rep.RestartSeconds)
		rep.Status, err = s.verify(ctx, srv)
		return err
	})
	return rep, err
}

func (s Store) withServer(ctx context.Context, pod string, token sensitive.Value, work func(Server) error) error {
	srv, closeFn, err := s.Dial(ctx, pod, token)
	if err != nil {
		return err
	}
	defer closeFn()
	return work(srv)
}

// pod zwraca pod StatefulSetu o nazwie name.
func (s Store) pod(ctx context.Context, name string) (cluster.Pod, bool, error) {
	pods, err := s.Cluster.Pods(ctx, store.Namespace, selector)
	if err != nil {
		return cluster.Pod{}, false, err
	}
	for _, p := range pods {
		if p.Name == name && !p.Terminating {
			return p, true, nil
		}
	}
	return cluster.Pod{}, false, nil
}

// activePod czeka na dokładnie jeden aktywny i gotowy pod.
func (s Store) activePod(ctx context.Context) (string, error) {
	var active string
	err := s.wait(ctx, "aktywny serwer OpenBao", func(ctx context.Context) (bool, error) {
		pods, err := s.Cluster.Pods(ctx, store.Namespace, selector+","+store.ActiveLabel+"=true")
		if err != nil {
			return false, err
		}
		var ready []string
		for _, p := range pods {
			if p.Ready && !p.Terminating {
				ready = append(ready, p.Name)
			}
		}
		if len(ready) != 1 {
			return false, nil
		}
		active = ready[0]
		return true, nil
	})
	return active, err
}

func (s Store) allRunning(ctx context.Context) (bool, error) {
	return s.count(ctx, func(p cluster.Pod) bool { return p.Running })
}

func (s Store) allReady(ctx context.Context) (bool, error) {
	return s.count(ctx, func(p cluster.Pod) bool { return p.Ready })
}

// count mówi, czy wszystkie repliki StatefulSetu spełniają ok. StatefulSetu
// może jeszcze nie być (chart dopiero zainstalowany) — wtedy czekamy dalej.
func (s Store) count(ctx context.Context, ok func(cluster.Pod) bool) (bool, error) {
	sts, err := s.Cluster.StatefulSet(ctx, store.Namespace, store.StatefulSet)
	if err != nil {
		return false, nil
	}
	pods, err := s.Cluster.Pods(ctx, store.Namespace, selector)
	if err != nil {
		return false, err
	}
	n := int32(0)
	for _, p := range pods {
		if ok(p) && !p.Terminating {
			n++
		}
	}
	return n == sts.Wanted, nil
}

func (s Store) wait(ctx context.Context, what string, check func(context.Context) (bool, error)) error {
	err := poll.Until(ctx, s.timeout(), 2*time.Second, check)
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("%s: warunek niespełniony po %v — kubectl -n %s get pods,events", what, s.timeout(), store.Namespace)
	}
	return err
}
