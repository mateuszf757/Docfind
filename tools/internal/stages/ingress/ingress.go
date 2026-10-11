// Package ingress to bramka wejścia do klastra (`dft check tls`) i warunek
// zakończenia Etapu 3: wymuszone odnowienie certyfikatu przechodzi bez
// ingerencji — proxy zaczyna podawać nowy certyfikat bez restartu i bez ani
// jednego nieudanego żądania (dawniej ci/check-tls.sh).
//
// Po drodze sprawdza resztę wejścia: trasę podpiętą pod Gateway, TLS
// zweryfikowany względem CA, przekierowanie HTTP→HTTPS, identyfikator
// żądania docierający do backendu i polityki na podach proxy, których nie
// widać w `helm template`, bo tworzy je kontroler w trakcie działania.
package ingress

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/core/policy"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

var requestID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// Cluster — klaster środowiska (adapters/kube).
type Cluster interface {
	Gateway(ctx context.Context, namespace, name string) (cluster.Gateway, error)
	Route(ctx context.Context, namespace, name string) (cluster.Route, error)
	Certificate(ctx context.Context, namespace, name string) (cluster.Certificate, error)
	SecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)
	Deployments(ctx context.Context, namespace, selector string) ([]map[string]any, error)
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	// RenewCertificate — odnowienie tak, jak zrobiłby to cert-manager przed
	// wygaśnięciem (cmctl renew).
	RenewCertificate(ctx context.Context, namespace, name string) error
}

// Edge — żądania do wejścia klastra na 127.0.0.1 hosta (adapters/httpclient).
type Edge interface {
	Get(ctx context.Context, r loopback.Request) (loopback.Response, error)
	Certificate(ctx context.Context, r loopback.Request) (*x509.Certificate, error)
}

// Config — parametry bramki z definicji środowiska.
type Config struct {
	GatewayNamespace string
	Gateway          string
	AppNamespace     string
	Route            string
	// HTTPPort i HTTPSPort — porty na 127.0.0.1 hosta (load balancer k3d).
	HTTPPort  int
	HTTPSPort int
	// RenewProduction — wymuszone odnowienie także na produkcyjnym Let's
	// Encrypt. Każde to nowy certyfikat, a limit to 5 identycznych na tydzień
	// — po jego wyczerpaniu domena przez tydzień nie dostanie certyfikatu,
	// także tego, który odnowiłby się sam przed wygaśnięciem.
	RenewProduction bool
	// AllowRenewal — definicja środowiska dopuszcza operacje niszczące.
	AllowRenewal bool
	// MinRequests — poniżej tej liczby pętla nie biegła naprawdę.
	MinRequests int
	// Settle — jak długo pętla żądań biegnie przed odnowieniem i po
	// podmianie certyfikatu; zero — 3 s.
	Settle time.Duration
}

func (c Config) settle() time.Duration {
	if c.Settle == 0 {
		return 3 * time.Second
	}
	return c.Settle
}

// Report — wynik bramki w JSON.
type Report struct {
	Hostname        string         `json:"hostname"`
	Issuer          string         `json:"issuer"`
	RequestID       string         `json:"request_id"`
	ProxyNodes      []string       `json:"proxy_nodes"`
	RenewalSkipped  string         `json:"renewal_skipped,omitempty"`
	SerialBefore    string         `json:"serial_before,omitempty"`
	SerialAfter     string         `json:"serial_after,omitempty"`
	RevisionBefore  int64          `json:"revision_before,omitempty"`
	RevisionAfter   int64          `json:"revision_after,omitempty"`
	Requests        int            `json:"requests"`
	Failed          int            `json:"failed"`
	Codes           map[string]int `json:"codes,omitempty"`
	ProxyRestarts   int32          `json:"proxy_restarts"`
	Unmet           []string       `json:"unmet"`
	FinishedSeconds float64        `json:"seconds"`
}

// Checker — jeden bieg bramki.
type Checker struct {
	Cluster Cluster
	Edge    Edge
	Cfg     Config
}

// Run sprawdza wejście i — jeśli wolno — odnowienie pod ruchem.
func (c *Checker) Run(ctx context.Context) (rep Report, err error) {
	started := time.Now()
	rep = Report{Unmet: []string{}}
	defer func() {
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
		rep.FinishedSeconds = time.Since(started).Seconds()
	}()

	gw, err := c.Cluster.Gateway(ctx, c.Cfg.GatewayNamespace, c.Cfg.Gateway)
	if err != nil {
		return rep, err
	}
	rep.Hostname, rep.Issuer = gw.HTTPSHostname, gw.Issuer
	secret := c.Cfg.Gateway + "-tls"
	cli.Step("host %s, wydawca %s, HTTPS na 127.0.0.1:%d", rep.Hostname, rep.Issuer, c.Cfg.HTTPSPort)
	if rep.Hostname == "" {
		return rep, cli.Unmet("Gateway %s nie ma listenera https z nazwą hosta", c.Cfg.Gateway)
	}

	if err := c.wait(ctx, 60*time.Second, func(ctx context.Context) (bool, error) {
		g, err := c.Cluster.Gateway(ctx, c.Cfg.GatewayNamespace, c.Cfg.Gateway)
		return g.Programmed, err
	}); err != nil {
		return rep, cli.Unmet("Gateway %s nie jest zaprogramowany: %v", c.Cfg.Gateway, err)
	}
	route, err := c.Cluster.Route(ctx, c.Cfg.AppNamespace, c.Cfg.Route)
	if err != nil {
		return rep, cli.Unmet("%v", err)
	}
	if route.Accepted != "True" || route.ResolvedRefs != "True" {
		return rep, cli.Unmet("HTTPRoute %s nie jest przyjęta przez Gateway (Accepted %s, ResolvedRefs %s)", c.Cfg.Route, route.Accepted, route.ResolvedRefs)
	}
	cli.Step("HTTPRoute przyjęta przez Gateway")
	if err := c.wait(ctx, 300*time.Second, func(ctx context.Context) (bool, error) {
		cert, err := c.Cluster.Certificate(ctx, c.Cfg.GatewayNamespace, secret)
		return cert.Ready, err
	}); err != nil {
		return rep, cli.Unmet("certyfikat %s nie jest gotowy — kubectl -n %s describe certificate %s (%v)", secret, c.Cfg.GatewayNamespace, secret, err)
	}

	trust, err := c.trust(ctx, rep.Issuer)
	if err != nil {
		return rep, err
	}
	// HTTPS, nagłówki: weryfikacja zawsze względem CA, nigdy bez niej —
	// wyjątek to Let's Encrypt staging, którego certyfikat główny celowo nie
	// jest w żadnym magazynie zaufania; tam sprawdzamy przynajmniej, że
	// certyfikat naprawdę przyszedł ze stagingu.
	version := c.https(rep.Hostname, trust)
	resp, err := c.Edge.Get(ctx, version)
	if err != nil {
		return rep, cli.Unmet("HTTPS do %s nie działa (weryfikacja TLS albo trasa): %v", rep.Hostname, err)
	}
	if resp.Status != 200 {
		return rep, cli.Unmet("GET /version przez HTTPS zwraca %d", resp.Status)
	}
	served, err := c.Edge.Certificate(ctx, version)
	if err != nil {
		return rep, cli.Unmet("odczyt certyfikatu proxy: %v", err)
	}
	switch rep.Issuer {
	case "letsencrypt-staging":
		if !strings.Contains(served.Issuer.String(), "(STAGING)") {
			return rep, cli.Unmet("wydawca to letsencrypt-staging, a podany certyfikat wystawił %s", served.Issuer)
		}
		cli.Step("HTTPS: 200, certyfikat ze stagingu Let's Encrypt (łańcuch z założenia niezaufany)")
	default:
		cli.Step("HTTPS: 200, łańcuch certyfikatu zweryfikowany (%s)", rep.Issuer)
	}
	rep.RequestID = resp.Header["X-Request-Id"]
	if !requestID.MatchString(rep.RequestID) {
		return rep, cli.Unmet("brak identyfikatora żądania z proxy w odpowiedzi API (X-Request-Id: %q)", rep.RequestID)
	}
	cli.Step("X-Request-Id nadany przez proxy dociera do backendu: %s", rep.RequestID)

	if err := c.checkRedirect(ctx, rep.Hostname); err != nil {
		return rep, err
	}
	nodes, err := c.checkProxyPods(ctx)
	rep.ProxyNodes = nodes
	if err != nil {
		return rep, err
	}

	switch {
	case rep.Issuer == "letsencrypt" && !c.Cfg.RenewProduction:
		rep.RenewalSkipped = "produkcyjny Let's Encrypt: limit 5 certyfikatów na tydzień"
		cli.Step("produkcyjny Let's Encrypt: odnowienie pod ruchem pominięte (limit 5 certyfikatów na tydzień)")
		return rep, nil
	case !c.Cfg.AllowRenewal:
		rep.RenewalSkipped = "środowisko nie dopuszcza operacji niszczących"
		cli.Step("odnowienie pod ruchem pominięte — środowisko nie dopuszcza operacji niszczących")
		return rep, nil
	}
	if err := c.renewUnderTraffic(ctx, &rep, secret, version, served); err != nil {
		return rep, err
	}
	cli.Step("warunek zakończenia Etapu 3 spełniony: odnowienie certyfikatu bez ingerencji i bez utraty żądania")
	return rep, nil
}

// wait czeka na warunek; błąd odczytu to kolejna próba, a po limicie czasu
// powód — ostatni błąd albo niespełniony warunek.
func (c *Checker) wait(ctx context.Context, timeout time.Duration, ready func(context.Context) (bool, error)) error {
	var last error
	err := poll.Until(ctx, timeout, time.Second, func(ctx context.Context) (bool, error) {
		ok, err := ready(ctx)
		last = err
		return err == nil && ok, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		if last != nil {
			return last
		}
		return fmt.Errorf("warunek nie jest spełniony po %v", timeout)
	}
	return err
}

// trust — względem czego weryfikować certyfikat proxy.
func (c *Checker) trust(ctx context.Context, issuer string) (*loopback.Trust, error) {
	switch issuer {
	case "docfind-internal-ca":
		data, err := c.Cluster.SecretData(ctx, "cert-manager", "docfind-root-ca")
		if err != nil {
			return nil, fmt.Errorf("certyfikat główny własnego CA: %w", err)
		}
		if len(data["ca.crt"]) == 0 {
			return nil, errors.New("certyfikat główny własnego CA: brak ca.crt")
		}
		return &loopback.Trust{RootCAs: data["ca.crt"]}, nil
	case "letsencrypt-staging":
		// Łańcuch stagingu nie zweryfikuje się z założenia; wystawcę
		// sprawdza Run na podanym certyfikacie.
		return &loopback.Trust{SkipVerify: true}, nil
	case "letsencrypt":
		// Systemowy magazyn zaufania.
		return &loopback.Trust{}, nil
	}
	return nil, cli.Unmet("nieznany wydawca %q", issuer)
}

// https — GET /version przez load balancer na 127.0.0.1, z nazwą hosta
// w SNI i nagłówku Host.
func (c *Checker) https(host string, trust *loopback.Trust) loopback.Request {
	return loopback.Request{Port: c.Cfg.HTTPSPort, Host: host, Path: "/version", TLS: trust, Timeout: 3 * time.Second}
}

func (c *Checker) checkRedirect(ctx context.Context, hostname string) error {
	resp, err := c.Edge.Get(ctx, loopback.Request{Port: c.Cfg.HTTPPort, Host: hostname, Path: "/version", Timeout: 3 * time.Second})
	if err != nil {
		return cli.Unmet("HTTP nie odpowiada: %v", err)
	}
	want := c.https(hostname, nil)
	want.TLS = &loopback.Trust{}
	if resp.Status != 301 || resp.Header["Location"] != want.URL() {
		return cli.Unmet("HTTP nie przekierowuje na HTTPS (otrzymano: %d %s, oczekiwano 301 %s)", resp.Status, resp.Header["Location"], want.URL())
	}
	cli.Step("HTTP → 301 → https://%s:%d", hostname, c.Cfg.HTTPSPort)
	return nil
}

func (c *Checker) proxySelector() string {
	return fmt.Sprintf("gateway.envoyproxy.io/owning-gateway-name=%s,gateway.envoyproxy.io/owning-gateway-namespace=%s", c.Cfg.Gateway, c.Cfg.GatewayNamespace)
}

// checkProxyPods sprawdza polityki na Deploymentach proxy, które tworzy
// kontroler Envoy Gateway w trakcie działania — w `helm template` ich nie
// ma, więc polityki z zadania test ich nie widzą — oraz rozłożenie podów
// proxy na węzły.
func (c *Checker) checkProxyPods(ctx context.Context) ([]string, error) {
	manifests, err := c.Cluster.Deployments(ctx, "envoy-gateway-system", c.proxySelector())
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return nil, cli.Unmet("nie znaleziono Deploymentu proxy dla Gateway %s", c.Cfg.Gateway)
	}
	if violations := policy.Check(manifests, policy.Options{RequireDigest: true}); len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintf(cli.Err, "POLITYKA (proxy): %s\n", v)
		}
		return nil, cli.Unmet("pody proxy łamią polityki (decyzje 16 i 20)")
	}
	pods, err := c.Cluster.Pods(ctx, "envoy-gateway-system", c.proxySelector())
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, p := range pods {
		if !slices.Contains(nodes, p.Node) {
			nodes = append(nodes, p.Node)
		}
	}
	slices.Sort(nodes)
	if len(nodes) < 2 {
		return nodes, cli.Unmet("pody proxy stoją na %d węźle — awaria węzła zabrałaby całe wejście", len(nodes))
	}
	cli.Step("pody proxy: obrazy z digestem, bez limitu CPU, na %d węzłach", len(nodes))
	return nodes, nil
}

func (c *Checker) renewUnderTraffic(ctx context.Context, rep *Report, secret string, version loopback.Request, before *x509.Certificate) error {
	// Przy ACME odnowienie to nowe wyzwanie DNS-01: rekord TXT w Cloudflare,
	// propagacja do publicznych resolwerów, walidacja po stronie Let's
	// Encrypt. Własne CA podpisuje od ręki.
	renewTimeout := 120 * time.Second
	if strings.HasPrefix(rep.Issuer, "letsencrypt") {
		renewTimeout = 420 * time.Second
	}
	rep.SerialBefore = before.SerialNumber.Text(16)
	cert, err := c.Cluster.Certificate(ctx, c.Cfg.GatewayNamespace, secret)
	if err != nil {
		return err
	}
	rep.RevisionBefore = cert.Revision
	restartsBefore, err := c.proxyRestarts(ctx)
	if err != nil {
		return err
	}
	cli.Step("przed odnowieniem: numer seryjny %s, rewizja %d", rep.SerialBefore, rep.RevisionBefore)

	// Pętla żądań: każde nowym połączeniem TLS, czyli nowym handshake'iem —
	// dokładnie tym momentem, w którym proxy podaje certyfikat.
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	var mu sync.Mutex
	codes := map[string]int{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for loopCtx.Err() == nil {
			code := "000"
			if resp, err := c.Edge.Get(loopCtx, version); err == nil {
				code = strconv.Itoa(resp.Status)
			} else if loopCtx.Err() != nil {
				return // przerwanie pętli, nie nieudane żądanie
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
			_ = poll.Sleep(loopCtx, 100*time.Millisecond)
		}
	}()
	if err := poll.Sleep(ctx, c.Cfg.settle()); err != nil {
		return err
	}

	cli.Step("cmctl renew %s/%s", c.Cfg.GatewayNamespace, secret)
	if err := c.Cluster.RenewCertificate(ctx, c.Cfg.GatewayNamespace, secret); err != nil {
		return err
	}
	var last cluster.Certificate
	err = poll.Until(ctx, renewTimeout, time.Second, func(ctx context.Context) (bool, error) {
		cert, err := c.Cluster.Certificate(ctx, c.Cfg.GatewayNamespace, secret)
		if err == nil {
			last = cert
		}
		return err == nil && cert.Revision > rep.RevisionBefore && cert.Ready, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("cert-manager nie odnowił certyfikatu w %v (rewizja %d) — kubectl -n %s get challenges,orders", renewTimeout, last.Revision, c.Cfg.GatewayNamespace)
	}
	if err != nil {
		return err
	}
	rep.RevisionAfter = last.Revision
	cli.Step("cert-manager odnowił certyfikat (rewizja %d)", rep.RevisionAfter)

	// Proxy podaje nowy certyfikat sam — Envoy Gateway śledzi Secret
	// i przekazuje go proxy przez SDS, bez restartu podów.
	err = poll.Until(ctx, 60*time.Second, time.Second, func(ctx context.Context) (bool, error) {
		after, err := c.Edge.Certificate(ctx, version)
		if err != nil || after.SerialNumber.Cmp(before.SerialNumber) == 0 {
			return false, nil
		}
		rep.SerialAfter = after.SerialNumber.Text(16)
		return true, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("proxy nadal podaje stary certyfikat 60 s po odnowieniu")
	}
	if err != nil {
		return err
	}
	cli.Step("proxy podaje nowy certyfikat: numer seryjny %s", rep.SerialAfter)

	if err := poll.Sleep(ctx, c.Cfg.settle()); err != nil {
		return err
	}
	stopLoop()
	wg.Wait()
	rep.Codes = codes
	for code, n := range codes {
		rep.Requests += n
		if code != "200" {
			rep.Failed += n
		}
	}
	cli.Step("żądań HTTPS w trakcie odnowienia: %d, nieudanych: %d", rep.Requests, rep.Failed)
	if rep.Requests < c.Cfg.MinRequests {
		return cli.Unmet("tylko %d żądań — pętla nie biegła wystarczająco długo (minimum %d)", rep.Requests, c.Cfg.MinRequests)
	}
	if rep.Failed > 0 {
		return cli.Unmet("%d z %d żądań nie powiodło się podczas odnowienia certyfikatu (kody: %v)", rep.Failed, rep.Requests, codes)
	}

	// Restarty w trakcie odnowienia, a nie od początku życia podów: restart
	// sprzed tygodnia (np. inotify, „Czego bym dziś nie powtórzył") nie mówi
	// nic o podmianie certyfikatu.
	restartsAfter, err := c.proxyRestarts(ctx)
	if err != nil {
		return err
	}
	rep.ProxyRestarts = restartsAfter - restartsBefore
	if rep.ProxyRestarts > 0 {
		return cli.Unmet("kontenery proxy restartowały się w trakcie odnowienia (%d razy) — podmiana certyfikatu nie powinna wymagać restartu", rep.ProxyRestarts)
	}
	return nil
}

func (c *Checker) proxyRestarts(ctx context.Context) (int32, error) {
	pods, err := c.Cluster.Pods(ctx, "envoy-gateway-system", c.proxySelector())
	if err != nil {
		return 0, err
	}
	var restarts int32
	for _, p := range pods {
		restarts += p.Restarts
	}
	return restarts, nil
}
