// Package secretstore to model magazynu sekretów platformy (OpenBao,
// decyzja 42): stan serwerów i konfiguracja zapisana jako kod
// w deploy/platform/openbao — audyt, montowania, metody uwierzytelniania,
// polityki i role. `dft` doprowadza do niej serwer przy każdym
// `cluster up`, więc konfiguracja wyklikana ręcznie znika przy następnym
// wdrożeniu, a nie żyje obok gita.
package secretstore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Miejsce OpenBao w klastrze — te same nazwy w deploy/platform/openbao.
const (
	Namespace   = "openbao"
	StatefulSet = "openbao"
	// SealSecret — klucz odpieczętowania (seal "static"), SealKey — jego pole.
	SealSecret = "openbao-seal"
	SealKey    = "key"
	// TLSSecret — certyfikat listenera od wewnętrznego CA; ca.crt to CA,
	// którym klient sprawdza serwer. ServerName — nazwa z certyfikatu.
	TLSSecret  = "openbao-tls"
	ServerName = "openbao.openbao.svc"
	Port       = 8200
	// Konto i rola, przez które dft administruje OpenBao po odwołaniu tokenu
	// root; Audience — odbiorca tokenu konta.
	AdminAccount = "openbao-admin"
	AdminRole    = "platform-admin"
	Audience     = "openbao"
	AuthMount    = "kubernetes"
	// ActiveLabel — etykieta aktywnego poda (service_registration kubernetes),
	// RevisionLabel — rewizja szablonu StatefulSetu na podzie.
	ActiveLabel   = "openbao-active"
	RevisionLabel = "controller-revision-hash"
)

// Pliki konfiguracji względem korzenia repozytorium.
const (
	Dir         = "deploy/platform/openbao"
	ConfigFile  = "config.yaml"
	PoliciesDir = "policies"
)

// SealStatus — stan jednego serwera (sys/seal-status).
type SealStatus struct {
	Initialized bool
	Sealed      bool
	// Type — rodzaj pieczęci: static (klucz z Secreta), shamir, KMS.
	Type    string
	Version string
}

// Init — wynik inicjalizacji. Klucz odzyskiwania i token root istnieją
// tylko w pamięci jako wartości poufne: pierwszy idzie do pliku poza
// repozytorium, drugi jest odwoływany po pierwszej konfiguracji.
type Init struct {
	RecoveryKeys []sensitive.Value
	RootToken    sensitive.Value
}

// RaftServer — członek klastra Raft.
type RaftServer struct {
	NodeID  string `json:"node_id"`
	Address string `json:"address"`
	Leader  bool   `json:"leader"`
	Voter   bool   `json:"voter"`
}

// Config — stan OpenBao jako kod.
type Config struct {
	Audit  []Audit `json:"audit"`
	Mounts []Mount `json:"mounts"`
	Auth   []Auth  `json:"auth"`
	Roles  []Role  `json:"roles"`
	// Policies — nazwa → treść HCL, z plików policies/<nazwa>.hcl.
	Policies map[string]string `json:"-"`
}

// Audit — urządzenie audytu, które ma działać (deklaruje je konfiguracja
// serwera, nie API).
type Audit struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// Mount — silnik sekretów.
type Mount struct {
	Path        string            `json:"path"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Options     map[string]string `json:"options"`
}

// Auth — metoda uwierzytelniania i jej konfiguracja.
type Auth struct {
	Path   string            `json:"path"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
}

// Role — rola metody kubernetes: kto (konto w przestrzeni nazw, token dla
// odbiorcy Audience) dostaje które polityki i na jak długo.
type Role struct {
	Name           string   `json:"name"`
	Auth           string   `json:"auth"`
	ServiceAccount string   `json:"serviceAccount"`
	Namespace      string   `json:"namespace"`
	Audience       string   `json:"audience"`
	Policies       []string `json:"policies"`
	TTL            string   `json:"ttl"`
}

// Body — rola w postaci API metody kubernetes. Bez polityki default:
// rola ma dokładnie to, co wypisano. token_max_ttl równy ttl — tokenu nie
// da się przedłużać bez końca.
func (r Role) Body() map[string]any {
	return map[string]any{
		"bound_service_account_names":      []string{r.ServiceAccount},
		"bound_service_account_namespaces": []string{r.Namespace},
		"audience":                         r.Audience,
		"token_policies":                   r.Policies,
		"token_no_default_policy":          true,
		"token_ttl":                        r.TTL,
		"token_max_ttl":                    r.TTL,
	}
}

var name = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`)

// LoadConfig czyta konfigurację z katalogu dir i sprawdza jej spójność.
func LoadConfig(dir string) (Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, PoliciesDir, "*.hcl"))
	if err != nil {
		return Config{}, err
	}
	c.Policies = map[string]string{}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return Config{}, err
		}
		c.Policies[strings.TrimSuffix(filepath.Base(f), ".hcl")] = string(body)
	}
	if problems := c.Validate(); len(problems) > 0 {
		return Config{}, fmt.Errorf("konfiguracja OpenBao (%s): %s", dir, strings.Join(problems, "; "))
	}
	return c, nil
}

// Validate zwraca niespójności: rola z polityką bez pliku albo z metodą,
// której nie ma, czas spoza zakresu, nazwy spoza dozwolonych znaków.
// Błąd tutaj zatrzymuje `dft test`, a nie wdrożenie w połowie.
func (c Config) Validate() []string {
	var problems []string
	paths := map[string]bool{}
	check := func(kind, p string) {
		switch {
		case !name.MatchString(p):
			problems = append(problems, fmt.Sprintf("%s %q: dozwolone [a-z0-9-]", kind, p))
		case paths[p]:
			problems = append(problems, fmt.Sprintf("ścieżka %q użyta dwa razy", p))
		}
		paths[p] = true
	}
	for _, a := range c.Audit {
		check("audyt", a.Path)
	}
	for _, m := range c.Mounts {
		check("montowanie", m.Path)
	}
	auth := map[string]string{}
	for _, a := range c.Auth {
		check("metoda", a.Path)
		auth[a.Path] = a.Type
	}
	for p, body := range c.Policies {
		if !name.MatchString(p) {
			problems = append(problems, fmt.Sprintf("polityka %q: dozwolone [a-z0-9-]", p))
		}
		if strings.TrimSpace(body) == "" {
			problems = append(problems, fmt.Sprintf("polityka %s: pusty plik", p))
		}
	}
	roles := map[string]bool{}
	for _, r := range c.Roles {
		where := "rola " + r.Name
		if !name.MatchString(r.Name) || roles[r.Name] {
			problems = append(problems, where+": nazwa spoza [a-z0-9-] albo powtórzona")
		}
		roles[r.Name] = true
		if auth[r.Auth] != "kubernetes" {
			problems = append(problems, fmt.Sprintf("%s: metoda %q nie jest metodą kubernetes z listy auth", where, r.Auth))
		}
		if r.ServiceAccount == "" || r.Namespace == "" {
			problems = append(problems, where+": bez konta albo przestrzeni nazw — rola dla każdego")
		}
		if r.Audience == "" {
			problems = append(problems, where+": bez audience — przyjęłaby token wydany dla API servera")
		}
		if len(r.Policies) == 0 {
			problems = append(problems, where+": bez polityk")
		}
		for _, p := range r.Policies {
			if _, ok := c.Policies[p]; !ok {
				problems = append(problems, fmt.Sprintf("%s: polityka %s bez pliku %s/%s.hcl", where, p, PoliciesDir, p))
			}
		}
		if ttl, err := time.ParseDuration(r.TTL); err != nil || ttl <= 0 || ttl > time.Hour {
			problems = append(problems, fmt.Sprintf("%s: ttl %q — oczekiwano czasu do 1h", where, r.TTL))
		}
	}
	// dft loguje się rolą administracyjną po odwołaniu tokenu root; bez niej
	// pierwsze wdrożenie odcięłoby go od serwera, który właśnie skonfigurował.
	if !slices.ContainsFunc(c.Roles, func(r Role) bool {
		return r.Name == AdminRole && r.ServiceAccount == AdminAccount && r.Namespace == Namespace && r.Audience == Audience
	}) {
		problems = append(problems, fmt.Sprintf("brak roli %s dla konta %s/%s z audience %s — dft straciłby dostęp", AdminRole, Namespace, AdminAccount, Audience))
	}
	slices.Sort(problems)
	return problems
}
