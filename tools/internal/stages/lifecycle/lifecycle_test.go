package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/ociarchive"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		values []float64
		want   float64
	}{
		{nil, 0},
		{[]float64{0.5}, 0.5},
		{[]float64{0.9, 0.1, 0.5}, 0.5},
		// Parzysta liczba pomiarów: średnia dwóch środkowych, jak
		// statistics.median w dawnym skrypcie.
		{[]float64{0.4, 0.1, 0.2, 0.3}, 0.25},
	}
	for _, tt := range tests {
		if got := median(tt.values); got != tt.want {
			t.Errorf("median(%v) = %v, oczekiwano %v", tt.values, got, tt.want)
		}
	}
}

func TestDefaultLimits(t *testing.T) {
	// Żądanie w locie musi być krótsze niż service.shutdown_grace_seconds
	// konfiguracji przykładowej (3 s) — inaczej bramka byłaby czerwona
	// z definicji.
	if DefaultLimits.InflightDelay >= 3*time.Second {
		t.Errorf("InflightDelay %v nie mieści się w shutdown_grace_seconds", DefaultLimits.InflightDelay)
	}
}

// engine — silnik kontenerów w pamięci: obraz odmawia startu bez poprawnej
// konfiguracji, a z poprawną odpowiada na HTTP.
type engine struct {
	mu   sync.Mutex
	size int64
	// user — USER obrazu; pusty w atrapie znaczy 10001.
	user    string
	refuse  container.Exit
	running map[string]container.Spec
	removed []string
	// hardenedFails — obraz nie wstaje z ustawieniami poda (pisze poza
	// emptyDir); hardened — kontenery uruchomione z ustawieniami poda.
	hardenedFails bool
	hardened      []string
}

// Save zapisuje prawdziwe archiwum obrazu z jedną warstwą: binarką
// o rozmiarze size bajtów (rozmiar liczy się z warstw archiwum).
func (e *engine) Save(_ context.Context, ref, path string) error {
	binary := filepath.Join(filepath.Dir(path), "app")
	if err := os.WriteFile(binary, make([]byte, e.size), 0o755); err != nil {
		return err
	}
	img, err := ociarchive.SingleBinary(binary, ociarchive.BinaryConfig{Path: "/app", User: 10001, Arch: "amd64"})
	if err != nil {
		return err
	}
	return img.Write(ref, path)
}

func (e *engine) ImageUser(context.Context, string) (string, error) {
	if e.user == "" {
		return "10001", nil
	}
	return e.user, nil
}

func (e *engine) RunToExit(_ context.Context, s container.Spec, _ time.Duration) (container.Exit, error) {
	if !s.NoNetwork {
		return container.Exit{}, errors.New("kontener odmowy z siecią")
	}
	exit := e.refuse
	if len(s.Mounts) == 1 {
		raw, err := os.ReadFile(filepath.Join(s.Mounts[0].Source, "app.yml"))
		if err != nil {
			return container.Exit{}, err
		}
		for _, field := range []string{"log_levl", "port: 0"} {
			if strings.Contains(string(raw), field) {
				exit.Stderr += " pole service." + strings.Fields(strings.ReplaceAll(field, ":", ""))[0]
			}
		}
	}
	return exit, nil
}

func (e *engine) Start(_ context.Context, s container.Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running == nil {
		e.running = map[string]container.Spec{}
	}
	e.running[s.Name] = s
	if s.Hardening != nil {
		e.hardened = append(e.hardened, s.Name)
	}
	return nil
}

func (e *engine) Stop(_ context.Context, name string, _ time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.running, name)
	return nil
}

func (e *engine) HostPort(_ context.Context, name string, port int) (int, error) {
	if port != 8000 {
		return 0, errors.New("zły port")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Kontener, który nie wstał, nie odpowiada: web odrzuca ten port.
	if e.hardenedFails && e.running[name].Hardening != nil {
		return 49154, nil
	}
	return 49153, nil
}

func (e *engine) Remove(_ context.Context, names ...string) error {
	e.removed = append(e.removed, names...)
	return nil
}

type web struct {
	status int
	// commit — commit w odpowiedzi /version.
	commit string
}

func (w web) Get(_ context.Context, r loopback.Request) (loopback.Response, error) {
	if r.Port != 49153 || r.TLS != nil {
		return loopback.Response{}, errors.New("zły adres")
	}
	resp := loopback.Response{Status: w.status, Elapsed: time.Millisecond}
	if r.Path == "/version" && r.ReadBody > 0 {
		resp.Body = []byte(`{"commit":"` + w.commit + `","version":"0.4.0"}`)
	}
	return resp, nil
}

func checker(t *testing.T, e *engine, w web) *Checker {
	t.Helper()
	limits := DefaultLimits
	limits.Repeats, limits.InflightDelay, limits.StartupTimeout = 1, 40*time.Millisecond, 50*time.Millisecond
	return &Checker{Containers: e, HTTP: w, Image: "img:1", ConfigExample: "../../../../deploy/config/app.yml.example", Limits: limits, Name: "dft-test"}
}

// TestRun: obraz spełniający warunki przechodzi, kontenery są sprzątane.
func TestRun(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	e := &engine{size: 4096, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}
	rep, err := checker(t, e, web{status: 200}).Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%+v)", err, rep.Unmet)
	}
	if len(rep.Refusals) != 3 || rep.Inflight == nil || !rep.Inflight.Completed || rep.Shutdown == nil {
		t.Errorf("raport %+v", rep)
	}
	if !slices.Contains(e.removed, "dft-test-w-locie") || len(e.running) != 0 {
		t.Errorf("sprzątanie: usunięte %v, działające %v", e.removed, e.running)
	}
}

func TestRunRejects(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	tests := []struct {
		name   string
		engine *engine
		web    web
		want   string
	}{
		{"obraz jako root", &engine{size: 1, user: "root", refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, web{status: 200}, "obraz uruchamia się jako root"},
		{"obraz jako 0:0", &engine{size: 1, user: "0:0", refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, web{status: 200}, "obraz uruchamia się jako root"},
		{"wstaje bez konfiguracji", &engine{size: 1, refuse: container.Exit{Code: -1, TimedOut: true}}, web{status: 200}, "brak konfiguracji: kontener wstał i działał"},
		{"odmowa bez komunikatu", &engine{size: 1, refuse: container.Exit{Code: 1, Stderr: "Traceback"}}, web{status: 200}, "odmowa startu bez czytelnego komunikatu"},
		{"nie odpowiada na /healthz", &engine{size: 1, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, web{status: 503}, "nie odpowiedział 200 na /healthz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := checker(t, tt.engine, tt.web).Run(context.Background())
			if cli.ExitCode(err) != cli.ExitUnmet || !slices.ContainsFunc(rep.Unmet, func(u string) bool { return strings.Contains(u, tt.want) }) {
				t.Errorf("oczekiwano niespełnionego z %q, jest %v (%v)", tt.want, err, rep.Unmet)
			}
		})
	}
}

// TestRunRejectsSize: rozmiar liczony z warstw archiwum — binarka 50 kB
// przy limicie 10 kB to obraz za duży, niezależnie od magazynu demona.
func TestRunRejectsSize(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	c := checker(t, &engine{size: 50_000, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, web{status: 200})
	c.Limits.MaxImageUnpackedBytes = 10_000
	rep, err := c.Run(context.Background())
	if cli.ExitCode(err) != cli.ExitUnmet || !slices.ContainsFunc(rep.Unmet, func(u string) bool { return strings.Contains(u, "limit 10000 B") }) {
		t.Errorf("obraz za duży: %v (%v)", err, rep.Unmet)
	}
	// Warstwa to strumień tar: binarka plus nagłówki i wyrównanie.
	if rep.ImageUnpackedBytes < 50_000 || rep.ImageUnpackedBytes > 52_000 {
		t.Errorf("rozmiar z warstw: %d", rep.ImageUnpackedBytes)
	}
}

var podSettings = &container.Hardening{ReadOnlyRoot: true, Tmpfs: []string{"/tmp"}, CapDropAll: true, NoNewPrivileges: true, User: "10001:10001"}

// TestRunPodSmoke: dym z ustawieniami poda — kontener uruchomiony z nimi,
// /version z commitem obrazu, wynik w raporcie.
func TestRunPodSmoke(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	e := &engine{size: 4096, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}
	c := checker(t, e, web{status: 200, commit: "abc"})
	c.Pod, c.Commit = podSettings, "abc"
	rep, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%+v)", err, rep.Unmet)
	}
	if rep.Pod == nil || !rep.Pod.Ready || rep.Pod.Commit != "abc" || rep.Pod.VersionStatus != 200 || !strings.Contains(rep.Pod.Settings, "tylko do odczytu") {
		t.Errorf("raport dymu: %+v", rep.Pod)
	}
	if !slices.Equal(e.hardened, []string{"dft-test-pod"}) || !slices.Contains(e.removed, "dft-test-pod") {
		t.Errorf("kontener z ustawieniami poda: %v, usunięte %v", e.hardened, e.removed)
	}
}

func TestRunPodSmokeRejects(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	for _, tt := range []struct {
		name   string
		engine *engine
		commit string
		want   string
	}{
		{"nie wstaje z ustawieniami poda", &engine{size: 1, hardenedFails: true, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, "abc", "z ustawieniami poda (system plików tylko do odczytu"},
		{"inny commit na /version", &engine{size: 1, refuse: container.Exit{Code: 1, Stderr: "BŁĄD KONFIGURACJI"}}, "zzz", `/version podaje commit "zzz", a obraz jest z "abc"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := checker(t, tt.engine, web{status: 200, commit: tt.commit})
			c.Pod, c.Commit = podSettings, "abc"
			rep, err := c.Run(context.Background())
			if cli.ExitCode(err) != cli.ExitUnmet || !slices.ContainsFunc(rep.Unmet, func(u string) bool { return strings.Contains(u, tt.want) }) {
				t.Errorf("oczekiwano niespełnionego z %q: %v (%q)", tt.want, err, rep.Unmet)
			}
		})
	}
}
