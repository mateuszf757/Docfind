package secretstore

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRepositoryConfig: konfiguracja z repozytorium jest spójna — rola
// administracyjna wskazuje politykę z pliku i metodę kubernetes.
func TestRepositoryConfig(t *testing.T) {
	c, err := LoadConfig(filepath.Join("..", "..", "..", "..", Dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Policies["platform-admin"]; !ok || len(c.Roles) == 0 {
		t.Errorf("konfiguracja: %+v", c)
	}
}

// TestValidate: każda z niespójności, które inaczej wyszłyby dopiero
// w połowie wdrożenia albo jako rola dla każdego.
func TestValidate(t *testing.T) {
	valid := func() Config {
		return Config{
			Auth:     []Auth{{Path: "kubernetes", Type: "kubernetes"}},
			Mounts:   []Mount{{Path: "docfind", Type: "kv"}},
			Policies: map[string]string{"admin": `path "x" { capabilities = ["read"] }`},
			Roles:    []Role{{Name: AdminRole, Auth: "kubernetes", ServiceAccount: AdminAccount, Namespace: Namespace, Audience: Audience, Policies: []string{"admin"}, TTL: "15m"}},
		}
	}
	if problems := valid().Validate(); len(problems) != 0 {
		t.Fatalf("poprawna konfiguracja odrzucona: %v", problems)
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"polityka bez pliku", func(c *Config) { c.Roles[0].Policies = []string{"brak"} }, "polityka brak bez pliku"},
		{"metoda spoza listy", func(c *Config) { c.Roles[0].Auth = "userpass" }, "nie jest metodą kubernetes"},
		{"rola dla każdego", func(c *Config) { c.Roles[0].Namespace = "" }, "rola dla każdego"},
		{"bez audience", func(c *Config) { c.Roles[0].Audience = "" }, "bez audience"},
		{"długi token", func(c *Config) { c.Roles[0].TTL = "24h" }, "do 1h"},
		{"pusta polityka", func(c *Config) { c.Policies["admin"] = " \n" }, "pusty plik"},
		{"powtórzona ścieżka", func(c *Config) { c.Audit = []Audit{{Path: "docfind", Type: "file"}} }, "użyta dwa razy"},
		{"bez roli administracyjnej", func(c *Config) { c.Roles[0].ServiceAccount = "inne" }, "dft straciłby dostęp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			if problems := strings.Join(c.Validate(), "; "); !strings.Contains(problems, tt.want) {
				t.Errorf("niespójności %q, oczekiwano %q", problems, tt.want)
			}
		})
	}
}

// TestRoleBody: rola bez polityki default i z tokenem, którego nie da się
// przedłużać ponad ttl.
func TestRoleBody(t *testing.T) {
	b := Role{Name: "r", ServiceAccount: "sa", Namespace: "ns", Audience: "openbao", Policies: []string{"p"}, TTL: "15m"}.Body()
	if b["token_no_default_policy"] != true || b["token_max_ttl"] != "15m" || b["audience"] != "openbao" {
		t.Errorf("rola: %v", b)
	}
}
