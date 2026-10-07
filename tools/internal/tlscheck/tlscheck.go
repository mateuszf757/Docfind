// Package tlscheck to warunek zakończenia Etapu 3: wymuszone odnowienie
// certyfikatu przechodzi bez ingerencji — proxy zaczyna podawać nowy
// certyfikat bez restartu i bez ani jednego nieudanego żądania (dawniej
// ci/check-tls.sh).
//
// Po drodze sprawdza resztę wejścia: trasę podpiętą pod Gateway, TLS
// zweryfikowany względem CA, przekierowanie HTTP→HTTPS, identyfikator
// żądania docierający do backendu i polityki na podach proxy, których nie
// widać w `helm template`, bo tworzy je kontroler w trakcie działania.
package tlscheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/policy"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

var (
	gatewayGVR     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	httpRouteGVR   = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	certificateGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	requestID      = regexp.MustCompile(`^[0-9a-f-]{36}$`)
)

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
	// KubectlArgs — --kubeconfig i --context dla cmctl.
	KubectlArgs []string
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
	Core    kubernetes.Interface
	Dynamic dynamic.Interface
	// Cmctl uruchamia `cmctl renew` — odnowienie tak, jak zrobiłby to
	// cert-manager przed wygaśnięciem.
	Cmctl proc.Runner
	Cfg   Config
}

// Run sprawdza wejście i — jeśli wolno — odnowienie pod ruchem.
func (c *Checker) Run(ctx context.Context) (rep Report, err error) {
	started := time.Now()
	rep = Report{Unmet: []string{}}
	defer func() { rep.FinishedSeconds = time.Since(started).Seconds() }()
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		rep.Unmet = append(rep.Unmet, msg)
		return cli.Unmet("%s", msg)
	}

	gw, err := c.Dynamic.Resource(gatewayGVR).Namespace(c.Cfg.GatewayNamespace).Get(ctx, c.Cfg.Gateway, metav1.GetOptions{})
	if err != nil {
		return rep, fmt.Errorf("odczyt Gateway %s/%s: %w", c.Cfg.GatewayNamespace, c.Cfg.Gateway, err)
	}
	rep.Hostname = httpsHostname(gw)
	rep.Issuer = gw.GetAnnotations()["cert-manager.io/cluster-issuer"]
	secret := c.Cfg.Gateway + "-tls"
	cli.Step("host %s, wydawca %s, HTTPS na 127.0.0.1:%d", rep.Hostname, rep.Issuer, c.Cfg.HTTPSPort)
	if rep.Hostname == "" {
		return rep, fail("Gateway %s nie ma listenera https z nazwą hosta", c.Cfg.Gateway)
	}

	if err := c.waitCondition(ctx, gatewayGVR, c.Cfg.GatewayNamespace, c.Cfg.Gateway, "Programmed", 60*time.Second); err != nil {
		return rep, fail("Gateway %s nie jest zaprogramowany: %v", c.Cfg.Gateway, err)
	}
	if err := c.checkRoute(ctx); err != nil {
		return rep, fail("%v", err)
	}
	cli.Step("HTTPRoute przyjęta przez Gateway")
	if err := c.waitCondition(ctx, certificateGVR, c.Cfg.GatewayNamespace, secret, "Ready", 300*time.Second); err != nil {
		return rep, fail("certyfikat %s nie jest gotowy — kubectl -n %s describe certificate %s (%v)", secret, c.Cfg.GatewayNamespace, secret, err)
	}

	tlsConfig, err := c.tlsConfig(ctx, rep.Issuer, rep.Hostname)
	if err != nil {
		return rep, err
	}
	client := c.httpsClient(tlsConfig)

	// HTTPS, nagłówki: weryfikacja zawsze względem CA, nigdy bez niej —
	// wyjątek to Let's Encrypt staging, którego certyfikat główny celowo nie
	// jest w żadnym magazynie zaufania; tam sprawdzamy przynajmniej, że
	// certyfikat naprawdę przyszedł ze stagingu.
	versionURL := fmt.Sprintf("https://%s:%d/version", rep.Hostname, c.Cfg.HTTPSPort)
	resp, err := get(ctx, client, versionURL)
	if err != nil {
		return rep, fail("HTTPS do %s nie działa (weryfikacja TLS albo trasa): %v", rep.Hostname, err)
	}
	if resp.StatusCode != http.StatusOK {
		return rep, fail("GET /version przez HTTPS zwraca %d", resp.StatusCode)
	}
	served, err := c.servedCertificate(ctx, tlsConfig)
	if err != nil {
		return rep, fail("odczyt certyfikatu proxy: %v", err)
	}
	switch rep.Issuer {
	case "letsencrypt-staging":
		if !strings.Contains(served.Issuer.String(), "(STAGING)") {
			return rep, fail("wydawca to letsencrypt-staging, a podany certyfikat wystawił %s", served.Issuer)
		}
		cli.Step("HTTPS: 200, certyfikat ze stagingu Let's Encrypt (łańcuch z założenia niezaufany)")
	default:
		cli.Step("HTTPS: 200, łańcuch certyfikatu zweryfikowany (%s)", rep.Issuer)
	}
	rep.RequestID = resp.Header.Get("X-Request-Id")
	if !requestID.MatchString(rep.RequestID) {
		return rep, fail("brak identyfikatora żądania z proxy w odpowiedzi API (X-Request-Id: %q)", rep.RequestID)
	}
	cli.Step("X-Request-Id nadany przez proxy dociera do backendu: %s", rep.RequestID)

	if err := c.checkRedirect(ctx, rep.Hostname); err != nil {
		return rep, fail("%v", err)
	}

	nodes, err := c.checkProxyPods(ctx)
	rep.ProxyNodes = nodes
	if err != nil {
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
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
	if err := c.renewUnderTraffic(ctx, &rep, secret, tlsConfig, served); err != nil {
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
		return rep, err
	}
	cli.Step("warunek zakończenia Etapu 3 spełniony: odnowienie certyfikatu bez ingerencji i bez utraty żądania")
	return rep, nil
}

func httpsHostname(gw *unstructured.Unstructured) string {
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	for _, l := range listeners {
		m, ok := l.(map[string]any)
		if !ok || m["name"] != "https" {
			continue
		}
		host, _ := m["hostname"].(string)
		return host
	}
	return ""
}

func (c *Checker) waitCondition(ctx context.Context, gvr schema.GroupVersionResource, ns, name, condition string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		obj, err := c.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && conditionTrue(obj, condition) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("warunek %s nie jest True po %v", condition, timeout)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func conditionTrue(obj *unstructured.Unstructured, condition string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		if m, ok := raw.(map[string]any); ok && m["type"] == condition {
			return m["status"] == "True"
		}
	}
	return false
}

func (c *Checker) checkRoute(ctx context.Context) error {
	route, err := c.Dynamic.Resource(httpRouteGVR).Namespace(c.Cfg.AppNamespace).Get(ctx, c.Cfg.Route, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("HTTPRoute %s/%s: %w", c.Cfg.AppNamespace, c.Cfg.Route, err)
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	status := map[string]string{"Accepted": "brak", "ResolvedRefs": "brak"}
	for _, p := range parents {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		conditions, _ := pm["conditions"].([]any)
		for _, raw := range conditions {
			if m, ok := raw.(map[string]any); ok {
				if t, _ := m["type"].(string); t == "Accepted" || t == "ResolvedRefs" {
					status[t], _ = m["status"].(string)
				}
			}
		}
	}
	if status["Accepted"] != "True" || status["ResolvedRefs"] != "True" {
		return fmt.Errorf("HTTPRoute %s nie jest przyjęta przez Gateway (Accepted %s, ResolvedRefs %s)", c.Cfg.Route, status["Accepted"], status["ResolvedRefs"])
	}
	return nil
}

func (c *Checker) tlsConfig(ctx context.Context, issuer, hostname string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}
	switch issuer {
	case "docfind-internal-ca":
		s, err := c.Core.CoreV1().Secrets("cert-manager").Get(ctx, "docfind-root-ca", metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("certyfikat główny własnego CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(s.Data["ca.crt"]) {
			return nil, errors.New("certyfikat główny własnego CA: nieczytelny ca.crt")
		}
		cfg.RootCAs = pool
	case "letsencrypt-staging":
		// Łańcuch stagingu nie zweryfikuje się z założenia; wystawcę
		// sprawdza Run na podanym certyfikacie.
		cfg.InsecureSkipVerify = true
	case "letsencrypt":
		// Systemowy magazyn zaufania.
	default:
		return nil, cli.Unmet("nieznany wydawca %q", issuer)
	}
	return cfg, nil
}

// httpsClient łączy się zawsze z 127.0.0.1:port, z nazwą hosta w SNI
// i nagłówku Host — jak `curl --resolve`. Każde żądanie to nowe połączenie
// TLS, czyli nowy handshake: dokładnie ten moment, w którym proxy podaje
// certyfikat. Pętla na keep-alive testowałaby stary handshake.
func (c *Checker) httpsClient(cfg *tls.Config) *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   cfg,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", c.Cfg.HTTPSPort))
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp, resp.Body.Close()
}

func (c *Checker) servedCertificate(ctx context.Context, cfg *tls.Config) (*x509.Certificate, error) {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", c.Cfg.HTTPSPort))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("proxy nie podał certyfikatu")
	}
	return certs[0], nil
}

func (c *Checker) checkRedirect(ctx context.Context, hostname string) error {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", c.Cfg.HTTPPort))
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := get(ctx, client, fmt.Sprintf("http://%s:%d/version", hostname, c.Cfg.HTTPPort))
	if err != nil {
		return fmt.Errorf("HTTP nie odpowiada: %w", err)
	}
	want := fmt.Sprintf("https://%s:%d/version", hostname, c.Cfg.HTTPSPort)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
		return fmt.Errorf("HTTP nie przekierowuje na HTTPS (otrzymano: %d %s, oczekiwano 301 %s)", resp.StatusCode, resp.Header.Get("Location"), want)
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
	deps, err := c.Core.AppsV1().Deployments("envoy-gateway-system").List(ctx, metav1.ListOptions{LabelSelector: c.proxySelector()})
	if err != nil {
		return nil, err
	}
	if len(deps.Items) == 0 {
		return nil, cli.Unmet("nie znaleziono Deploymentu proxy dla Gateway %s", c.Cfg.Gateway)
	}
	var manifests []map[string]any
	for _, d := range deps.Items {
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		// Obiekt z API nie ma apiVersion i kind (lista je gubi) — polityki
		// rozpoznają rodzaj zasobu po kind.
		m["kind"] = "Deployment"
		manifests = append(manifests, m)
	}
	if violations := policy.Check(manifests, policy.Options{RequireDigest: true}); len(violations) > 0 {
		for _, v := range violations {
			fmt.Fprintf(cli.Err, "POLITYKA (proxy): %s\n", v)
		}
		return nil, cli.Unmet("pody proxy łamią polityki (decyzje 16 i 20)")
	}
	pods, err := c.Core.CoreV1().Pods("envoy-gateway-system").List(ctx, metav1.ListOptions{LabelSelector: c.proxySelector()})
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, p := range pods.Items {
		if !slices.Contains(nodes, p.Spec.NodeName) {
			nodes = append(nodes, p.Spec.NodeName)
		}
	}
	slices.Sort(nodes)
	if len(nodes) < 2 {
		return nodes, cli.Unmet("pody proxy stoją na %d węźle — awaria węzła zabrałaby całe wejście", len(nodes))
	}
	cli.Step("pody proxy: obrazy z digestem, bez limitu CPU, na %d węzłach", len(nodes))
	return nodes, nil
}

func (c *Checker) revision(ctx context.Context, secret string) (int64, bool, error) {
	cert, err := c.Dynamic.Resource(certificateGVR).Namespace(c.Cfg.GatewayNamespace).Get(ctx, secret, metav1.GetOptions{})
	if err != nil {
		return 0, false, err
	}
	rev, _, _ := unstructured.NestedInt64(cert.Object, "status", "revision")
	return rev, conditionTrue(cert, "Ready"), nil
}

func (c *Checker) renewUnderTraffic(ctx context.Context, rep *Report, secret string, cfg *tls.Config, before *x509.Certificate) error {
	// Przy ACME odnowienie to nowe wyzwanie DNS-01: rekord TXT w Cloudflare,
	// propagacja do publicznych resolwerów, walidacja po stronie Let's
	// Encrypt. Własne CA podpisuje od ręki.
	renewTimeout := 120 * time.Second
	if strings.HasPrefix(rep.Issuer, "letsencrypt") {
		renewTimeout = 420 * time.Second
	}
	rep.SerialBefore = before.SerialNumber.Text(16)
	revBefore, _, err := c.revision(ctx, secret)
	if err != nil {
		return err
	}
	rep.RevisionBefore = revBefore
	restartsBefore, err := c.proxyRestarts(ctx)
	if err != nil {
		return err
	}
	cli.Step("przed odnowieniem: numer seryjny %s, rewizja %d", rep.SerialBefore, revBefore)

	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	var mu sync.Mutex
	codes := map[string]int{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		client := c.httpsClient(cfg)
		url := fmt.Sprintf("https://%s:%d/version", rep.Hostname, c.Cfg.HTTPSPort)
		for loopCtx.Err() == nil {
			code := "000"
			if resp, err := get(loopCtx, client, url); err == nil {
				code = fmt.Sprint(resp.StatusCode)
			} else if loopCtx.Err() != nil {
				return // przerwanie pętli, nie nieudane żądanie
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
			select {
			case <-loopCtx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	if err := sleep(ctx, 3*time.Second); err != nil {
		return err
	}

	cli.Step("cmctl renew %s/%s", c.Cfg.GatewayNamespace, secret)
	args := append([]string{"renew", "--namespace", c.Cfg.GatewayNamespace, secret}, c.Cfg.KubectlArgs...)
	if _, err := c.Cmctl.Run(ctx, proc.Cmd{Name: "cmctl", Args: args}); err != nil {
		return fmt.Errorf("cmctl renew: %w", err)
	}
	deadline := time.Now().Add(renewTimeout)
	for {
		rev, ready, err := c.revision(ctx, secret)
		if err == nil && rev > revBefore && ready {
			rep.RevisionAfter = rev
			break
		}
		if time.Now().After(deadline) {
			return cli.Unmet("cert-manager nie odnowił certyfikatu w %v (rewizja %d) — kubectl -n %s get challenges,orders", renewTimeout, rev, c.Cfg.GatewayNamespace)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	cli.Step("cert-manager odnowił certyfikat (rewizja %d)", rep.RevisionAfter)

	// Proxy podaje nowy certyfikat sam — Envoy Gateway śledzi Secret
	// i przekazuje go proxy przez SDS, bez restartu podów.
	deadline = time.Now().Add(60 * time.Second)
	for {
		after, err := c.servedCertificate(ctx, cfg)
		if err == nil && after.SerialNumber.Cmp(before.SerialNumber) != 0 {
			rep.SerialAfter = after.SerialNumber.Text(16)
			break
		}
		if time.Now().After(deadline) {
			return cli.Unmet("proxy nadal podaje stary certyfikat 60 s po odnowieniu")
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	cli.Step("proxy podaje nowy certyfikat: numer seryjny %s", rep.SerialAfter)

	if err := sleep(ctx, 3*time.Second); err != nil {
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
	pods, err := c.Core.CoreV1().Pods("envoy-gateway-system").List(ctx, metav1.ListOptions{LabelSelector: c.proxySelector()})
	if err != nil {
		return 0, err
	}
	var restarts int32
	for _, p := range pods.Items {
		for _, s := range p.Status.ContainerStatuses {
			restarts += s.RestartCount
		}
	}
	return restarts, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
