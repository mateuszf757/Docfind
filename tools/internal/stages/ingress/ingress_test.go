package ingress

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// fakeIngress — klaster i wejście w jednym: odnowienie podbija rewizję
// certyfikatu, a proxy od razu podaje certyfikat z nowym numerem seryjnym.
type fakeIngress struct {
	mu         sync.Mutex
	gateway    cluster.Gateway
	route      cluster.Route
	revision   int64
	serial     int64
	proxyNodes []string
	restarts   int32
	cpuLimit   bool
	// failing — kod odpowiedzi HTTPS w trakcie odnowienia (0 — 200).
	failing  int
	renewed  bool
	issuerCN string
	redirect string
	// stale — co który handshake po odnowieniu podaje stary certyfikat
	// (replika bez nowego); 0 — żaden.
	stale     int
	handshake int
	// restarted — chwila restartu Deploymentu; outage — okno błędów po nim;
	// stall — jedno żądanie po restarcie czeka tyle, zanim odpowie.
	restarted time.Time
	outage    time.Duration
	stall     time.Duration
	stalled   bool
}

func (f *fakeIngress) RestartDeployment(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = time.Now()
	return nil
}

func (f *fakeIngress) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done := !f.restarted.IsZero()
	r := cluster.Rollout{Wanted: 2, Updated: 2, Available: 2, Total: 2, Observed: done}
	return r, nil
}

func newIngress() *fakeIngress {
	return &fakeIngress{
		gateway:    cluster.Gateway{HTTPSHostname: "local.docfind.lol", Issuer: "docfind-internal-ca", Programmed: true},
		route:      cluster.Route{Accepted: "True", ResolvedRefs: "True"},
		revision:   1,
		serial:     100,
		proxyNodes: []string{"agent-0", "agent-1"},
		issuerCN:   "docfind-root-ca",
		redirect:   "https://local.docfind.lol:8443/version",
	}
}

func (f *fakeIngress) Gateway(context.Context, string, string) (cluster.Gateway, error) {
	return f.gateway, nil
}
func (f *fakeIngress) Route(context.Context, string, string) (cluster.Route, error) {
	return f.route, nil
}
func (f *fakeIngress) Certificate(context.Context, string, string) (cluster.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cluster.Certificate{Revision: f.revision, Ready: true}, nil
}
func (f *fakeIngress) SecretData(context.Context, string, string) (map[string][]byte, error) {
	return map[string][]byte{"ca.crt": []byte("PEM")}, nil
}
func (f *fakeIngress) Deployments(context.Context, string, string) ([]map[string]any, error) {
	limits := map[string]any{"memory": "1Gi"}
	if f.cpuLimit {
		limits["cpu"] = "500m"
	}
	return []map[string]any{{"kind": "Deployment", "metadata": map[string]any{"name": "envoy"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{map[string]any{"name": "envoy", "image": "envoyproxy/envoy:v1@sha256:abc",
			"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}, "limits": limits}}},
	}}}}}, nil
}
func (f *fakeIngress) Pods(context.Context, string, string) ([]cluster.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pods []cluster.Pod
	for _, n := range f.proxyNodes {
		pods = append(pods, cluster.Pod{Name: "envoy-" + n, Node: n, Restarts: f.restarts})
	}
	return pods, nil
}
func (f *fakeIngress) RenewCertificate(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed = true
	f.revision++
	f.serial++
	return nil
}

func (f *fakeIngress) Get(_ context.Context, r loopback.Request) (loopback.Response, error) {
	f.mu.Lock()
	if !f.restarted.IsZero() && f.stall > 0 && !f.stalled {
		f.stalled = true
		stall := f.stall
		f.mu.Unlock()
		time.Sleep(stall)
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	if !f.restarted.IsZero() && time.Since(f.restarted) < f.outage {
		return loopback.Response{}, errors.New("connection refused")
	}
	if r.TLS == nil {
		return loopback.Response{Status: 301, Header: map[string]string{"Location": f.redirect}}, nil
	}
	// Zaufanie według wydawcy: własne CA — jego certyfikat, staging — bez
	// weryfikacji łańcucha.
	trusted := string(r.TLS.RootCAs) == "PEM" || (r.TLS.SkipVerify && f.gateway.Issuer == "letsencrypt-staging")
	if r.Host != "local.docfind.lol" || r.Port != 8443 || !trusted {
		return loopback.Response{}, errors.New("zła nazwa, port albo zaufanie")
	}
	status := 200
	if f.renewed && f.failing != 0 {
		status = f.failing
	}
	return loopback.Response{Status: status, Header: map[string]string{"X-Request-Id": "0f0e8d6a-5c4b-4a39-8f2e-1d0c9b8a7f6e"}}, nil
}

// edge — strona wejścia atrapy: Certificate wejścia koliduje nazwą
// z Certificate klastra, więc to osobny typ nad tym samym stanem.
type edge struct{ *fakeIngress }

func (e edge) Certificate(context.Context, loopback.Request) (*x509.Certificate, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	serial := e.serial
	if e.renewed && e.stale > 0 {
		e.handshake++
		if e.handshake%e.stale == 0 {
			serial--
		}
	}
	return &x509.Certificate{SerialNumber: big.NewInt(serial), Issuer: pkix.Name{CommonName: e.issuerCN}}, nil
}

func checker(f *fakeIngress) *Checker {
	return &Checker{Cluster: f, Edge: edge{f}, Cfg: Config{
		GatewayNamespace: "gateway", Gateway: "docfind", AppNamespace: "docfind", Route: "docfind-api",
		HTTPPort: 8080, HTTPSPort: 8443, AllowRenewal: true, MinRequests: 1, Settle: 50 * time.Millisecond,
		Propagation: 3 * time.Second, ConfirmInterval: time.Millisecond,
	}}
}

func TestRun(t *testing.T) {
	quiet(t)
	f := newIngress()
	rep, err := checker(f).Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%+v)", err, rep)
	}
	if rep.SerialBefore != "64" || rep.SerialAfter != "65" || rep.RevisionAfter != 2 || rep.Failed != 0 || rep.Requests == 0 || rep.Confirmed != 20 {
		t.Errorf("raport %+v", rep)
	}
	if len(rep.ProxyNodes) != 2 || rep.RenewalSkipped != "" {
		t.Errorf("proxy %v, pominięte %q", rep.ProxyNodes, rep.RenewalSkipped)
	}
}

func TestRunSkipsRenewal(t *testing.T) {
	quiet(t)
	f := newIngress()
	c := checker(f)
	c.Cfg.AllowRenewal = false
	rep, err := c.Run(context.Background())
	if err != nil || f.renewed || !strings.Contains(rep.RenewalSkipped, "operacji niszczących") {
		t.Errorf("bez zgody na odnowienie: %v, odnowiony %v, %q", err, f.renewed, rep.RenewalSkipped)
	}
}

func TestRunRejects(t *testing.T) {
	quiet(t)
	tests := []struct {
		name    string
		prepare func(*fakeIngress, *Checker)
		want    string
	}{
		{"trasa nieprzyjęta", func(f *fakeIngress, _ *Checker) { f.route.ResolvedRefs = "False" }, "nie jest przyjęta przez Gateway"},
		{"Gateway bez hosta https", func(f *fakeIngress, _ *Checker) { f.gateway.HTTPSHostname = "" }, "nie ma listenera https"},
		{"brak przekierowania", func(f *fakeIngress, _ *Checker) { f.redirect = "http://local.docfind.lol/version" }, "nie przekierowuje na HTTPS"},
		{"proxy z limitem CPU", func(f *fakeIngress, _ *Checker) { f.cpuLimit = true }, "łamią polityki"},
		{"proxy na jednym węźle", func(f *fakeIngress, _ *Checker) { f.proxyNodes = []string{"agent-0"} }, "na 1 węźle"},
		// Okno po podmianie dłuższe niż odstęp pętli (100 ms): żądania po
		// odnowieniu na pewno padną.
		{"nieudane żądania w trakcie odnowienia", func(f *fakeIngress, c *Checker) { f.failing, c.Cfg.Settle = 503, 350*time.Millisecond }, "nie powiodło się podczas odnowienia"},
		{"staging z cudzym wystawcą", func(f *fakeIngress, _ *Checker) { f.gateway.Issuer = "letsencrypt-staging" }, "a podany certyfikat wystawił"},
		{"nieznany wydawca", func(f *fakeIngress, _ *Checker) { f.gateway.Issuer = "selfsigned" }, "nieznany wydawca"},
		// Co piąty handshake trafia na replikę ze starym certyfikatem:
		// 20 kolejnych nowych nigdy się nie zbierze.
		{"replika proxy ze starym certyfikatem", func(f *fakeIngress, c *Checker) { f.stale, c.Cfg.Propagation = 5, 300*time.Millisecond }, "któraś replika proxy podaje stary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newIngress()
			c := checker(f)
			tt.prepare(f, c)
			rep, err := c.Run(context.Background())
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("oczekiwano niespełnionego z %q, jest %v", tt.want, err)
			}
			if len(rep.Unmet) != 1 || !strings.Contains(rep.Unmet[0], tt.want) {
				t.Errorf("raport bez powodu: %v", rep.Unmet)
			}
		})
	}
}

func TestRollout(t *testing.T) {
	quiet(t)
	f := newIngress()
	rep, err := checker(f).Rollout(context.Background(), "docfind-api", 10*time.Second)
	if err != nil {
		t.Fatalf("%v (%+v)", err, rep)
	}
	if rep.Requests < 100 || rep.Failed != 0 || rep.MaxGapSeconds > 0.5 || rep.Codes["200"] != rep.Requests {
		t.Errorf("raport %+v", rep)
	}
	for name, tt := range map[string]struct {
		prepare func(*fakeIngress, *Checker)
		want    string
	}{
		"błędy w trakcie rolloutu": {func(f *fakeIngress, _ *Checker) { f.outage = 200 * time.Millisecond }, "żądań nie powiodło się"},
		// Odpowiedź wraca po 1,2 s, zanim pętla się zatrzyma: przerwa bez
		// błędu, ale dłuższa niż Z.
		"przerwa dłuższa niż Z bez błędu": {func(f *fakeIngress, c *Checker) {
			f.stall, c.Cfg.Settle = 1200*time.Millisecond, 1500*time.Millisecond
		}, "przestój 1.2"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newIngress()
			c := checker(f)
			tt.prepare(f, c)
			rep, err := c.Rollout(context.Background(), "docfind-api", 10*time.Second)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) || len(rep.Unmet) != 1 {
				t.Errorf("oczekiwano niespełnionego z %q, jest %v (%+v)", tt.want, err, rep)
			}
		})
	}
}

// UnderTraffic z dowolną akcją: akcja biegnie pod ruchem, jej błąd
// przerywa pomiar, a raport ma jej nazwę.
func TestUnderTraffic(t *testing.T) {
	quiet(t)
	f := newIngress()
	c := checker(f)
	var called bool
	rep, err := c.UnderTraffic(context.Background(), "upgrade do N-1", "docfind-api", 10*time.Second, func(ctx context.Context) error {
		called = true
		return f.RestartDeployment(ctx, c.Cfg.AppNamespace, "docfind-api")
	})
	if err != nil || !called || rep.Action != "upgrade do N-1" || rep.Requests < 100 {
		t.Errorf("akcja pod ruchem: %v, %v (%+v)", err, called, rep)
	}
	_, err = c.UnderTraffic(context.Background(), "rollback", "docfind-api", 10*time.Second, func(context.Context) error {
		return errors.New("helm rollback: kod 1")
	})
	if err == nil || !strings.Contains(err.Error(), "helm rollback") {
		t.Errorf("błąd akcji: %v", err)
	}
}

func TestMaxGap(t *testing.T) {
	at := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	samples := []sample{{at, true}, {at.Add(100 * time.Millisecond), false}, {at.Add(700 * time.Millisecond), true}, {at.Add(800 * time.Millisecond), true}}
	if gap, failed := maxGap(samples); gap != 700*time.Millisecond || failed != 1 {
		t.Errorf("maxGap = %v, %d", gap, failed)
	}
}
