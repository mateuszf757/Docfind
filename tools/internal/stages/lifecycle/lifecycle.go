// Package lifecycle to warunki zakończenia Etapu 1 sprawdzane na
// zbudowanym obrazie (`dft check runtime`, dawniej ci/check-runtime.sh):
// rozmiar, czytelna odmowa startu przy złej konfiguracji (U11b), koszt
// zamykania i żądanie w locie przy SIGTERM (U3) — cykl życia kontenera od
// startu do zatrzymania.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

// Containers — lokalny silnik kontenerów (adapters/docker).
type Containers interface {
	ImageSize(ctx context.Context, ref string) (int64, error)
	RunToExit(ctx context.Context, s container.Spec, timeout time.Duration) (container.Exit, error)
	Start(ctx context.Context, s container.Spec) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	HostPort(ctx context.Context, name string, containerPort int) (int, error)
	Remove(ctx context.Context, names ...string) error
}

// HTTP — żądania do kontenera na 127.0.0.1 (adapters/httpclient).
type HTTP interface {
	Get(ctx context.Context, r loopback.Request) (loopback.Response, error)
}

// Limits — progi bramki. Jednostka i miara są w nazwie, bo „200 MB" znaczyło
// w tym projekcie trzy różne rzeczy: rozpakowany obraz w magazynie Dockera
// (tu), skompresowane warstwy w rejestrze (~29 MB dla tego samego obrazu)
// i „200 MiB" w politykach klastra z zewnętrznej analizy.
type Limits struct {
	// MaxImageUnpackedBytes — rozmiar obrazu rozpakowanego w magazynie Dockera
	// (`docker image inspect .Size`), w bajtach. Warunek Etapu 1.
	MaxImageUnpackedBytes int64
	// MaxOwnShutdown — koszt własny zamykania: mediana `docker stop` aplikacji
	// minus mediana odniesienia. Zmierzony koszt uvicorna na Alpine to
	// 0,26–0,50 s; próg na granicy dawałby test migoczący co drugi przebieg,
	// a test, który zawodzi losowo, uczy ignorowania czerwonego wyniku. Próg
	// ma rozróżniać awarię, a ta wygląda inaczej: proces bez obsługi SIGTERM
	// czeka cały limit `docker stop` do SIGKILL.
	MaxOwnShutdown time.Duration
	// TotalShutdownAdvisory — orientacyjny próg całkowitego czasu
	// `docker stop`; tylko ostrzeżenie, bo narzut demona Dockera waha się
	// między przebiegami i mówi o maszynie, nie o naszym kodzie.
	TotalShutdownAdvisory time.Duration
	// StopTimeout — ile `docker stop` czeka przed SIGKILL.
	StopTimeout time.Duration
	// StartupTimeout — ile czekać na /healthz po starcie kontenera.
	StartupTimeout time.Duration
	// RefusalTimeout — po tym czasie kontener bez konfiguracji uznaje się za
	// taki, który wstał, zamiast odmówić. Wcześniej skrypt czekał bez końca.
	RefusalTimeout time.Duration
	// Repeats — liczba pomiarów odniesienia i aplikacji (mediana).
	Repeats int
	// InflightDelay — czas trwania żądania w locie (llm.stub_delay_ms);
	// musi być krótszy niż service.shutdown_grace_seconds z konfiguracji
	// przykładowej (3 s), żeby uvicorn je dokończył.
	InflightDelay time.Duration
}

// DefaultLimits to progi warunków Etapu 1.
var DefaultLimits = Limits{
	MaxImageUnpackedBytes: contract.MaxImageUnpackedBytes,
	MaxOwnShutdown:        contract.MaxOwnShutdown,
	TotalShutdownAdvisory: time.Second,
	StopTimeout:           10 * time.Second,
	StartupTimeout:        20 * time.Second,
	RefusalTimeout:        contract.RefusalTimeout,
	Repeats:               5,
	InflightDelay:         contract.InflightDelay,
}

// Checker sprawdza jeden obraz.
type Checker struct {
	Containers Containers
	HTTP       HTTP
	// Image — obraz:tag do sprawdzenia.
	Image string
	// ConfigExample — deploy/config/app.yml.example, konfiguracja startowa.
	ConfigExample string
	Limits        Limits
	// Entrypoint zastępuje ENTRYPOINT obrazu — tylko dla wariantów
	// negatywnych w testach (obraz, który wstaje bez konfiguracji; proces bez
	// obsługi SIGTERM). Pusty: obraz uruchamiany tak, jak w produkcji.
	Entrypoint []string
	// Name — przedrostek nazw kontenerów; nazwy są unikalne dla przebiegu,
	// żeby dwa równoległe biegi nie zatrzymywały sobie kontenerów.
	Name string

	containers []string
}

// Report to wynik bramki w JSON — dowód zostaje po biegu (U14).
type Report struct {
	Image                 string    `json:"image"`
	ImageUnpackedBytes    int64     `json:"image_unpacked_bytes"`
	MaxImageUnpackedBytes int64     `json:"max_image_unpacked_bytes"`
	Refusals              []Refusal `json:"refusals"`
	Shutdown              *Shutdown `json:"shutdown,omitempty"`
	Inflight              *Inflight `json:"inflight,omitempty"`
	Unmet                 []string  `json:"unmet"`
	Finished              time.Time `json:"finished"`
}

// Refusal — jedna próba startu bez poprawnej konfiguracji.
type Refusal struct {
	Case     string `json:"case"`
	ExitCode int    `json:"exit_code"`
	Refused  bool   `json:"refused"`
	Output   string `json:"output"`
}

// Shutdown — pomiar kosztu zamykania.
type Shutdown struct {
	BaselineSeconds []float64 `json:"baseline_seconds"`
	TotalSeconds    []float64 `json:"total_seconds"`
	BaselineMedian  float64   `json:"baseline_median_seconds"`
	TotalMedian     float64   `json:"total_median_seconds"`
	OwnSeconds      float64   `json:"own_seconds"`
	MaxOwnSeconds   float64   `json:"max_own_seconds"`
}

// Inflight — żądanie w toku w chwili SIGTERM.
type Inflight struct {
	DelaySeconds   float64 `json:"delay_seconds"`
	Status         int     `json:"status"`
	Error          string  `json:"error,omitempty"`
	RequestSeconds float64 `json:"request_seconds"`
	StopSeconds    float64 `json:"stop_seconds"`
	Completed      bool    `json:"completed_after_sigterm"`
}

// Run sprawdza wszystkie warunki i zgłasza niespełnione razem.
func (c *Checker) Run(ctx context.Context) (rep Report, err error) {
	rep = Report{Image: c.Image, MaxImageUnpackedBytes: c.Limits.MaxImageUnpackedBytes}
	v := &cli.Verdict{}
	fail := func(format string, args ...any) {
		v.Fail(format, args...)
		rep.Unmet = append(rep.Unmet, fmt.Sprintf(format, args...))
	}
	// Kontenery testowe sprzątane zawsze, także po SIGINT: kontekst może być
	// już anulowany, więc sprzątanie ma własny.
	defer func() {
		cleanupCtx, cancel := cli.CleanupContext(30 * time.Second)
		defer cancel()
		err = errors.Join(err, c.removeContainers(cleanupCtx))
		rep.Finished = time.Now().UTC()
	}()

	tmp, err := os.MkdirTemp("", "dft-runtime-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)

	cli.Step("sprawdzany obraz: %s", c.Image)
	if err := c.checkSize(ctx, &rep, fail); err != nil {
		return rep, err
	}
	if err := c.checkRefusals(ctx, tmp, &rep, fail); err != nil {
		return rep, err
	}
	if err := c.checkShutdown(ctx, tmp, &rep, fail); err != nil {
		return rep, err
	}
	if err := c.checkInflight(ctx, tmp, &rep, fail); err != nil {
		return rep, err
	}
	if rep.Unmet == nil {
		rep.Unmet = []string{}
	}
	return rep, v.Err("warunki zakończenia Etapu 1")
}

func (c *Checker) checkSize(ctx context.Context, rep *Report, fail func(string, ...any)) error {
	size, err := c.Containers.ImageSize(ctx, c.Image)
	if err != nil {
		return err
	}
	rep.ImageUnpackedBytes = size
	limit := c.Limits.MaxImageUnpackedBytes
	if size > limit {
		fail("obraz rozpakowany w magazynie Dockera ma %d B (%.1f MB), limit %d B", size, float64(size)/1e6, limit)
		return nil
	}
	cli.Step("rozmiar obrazu rozpakowanego w magazynie Dockera: %d B (%.1f MB), limit %d B", size, float64(size)/1e6, limit)
	return nil
}

// refusalCase to jedna próba startu, która ma się skończyć odmową.
type refusalCase struct {
	name string
	// config — treść app.yml; pusta oznacza brak konfiguracji.
	config string
	// field — pole, które komunikat ma wskazać.
	field string
}

func (c *Checker) checkRefusals(ctx context.Context, tmp string, rep *Report, fail func(string, ...any)) error {
	example, err := os.ReadFile(c.ConfigExample)
	if err != nil {
		return fmt.Errorf("konfiguracja przykładowa: %w", err)
	}
	// Obiecane w README Etapu 1: zły config = czytelna odmowa startu, z polem,
	// które jest złe. Bramka sprawdzała tylko brak konfiguracji (U11b).
	cases := []refusalCase{
		{name: "brak konfiguracji"},
		{name: "nieznany klucz", config: strings.Replace(string(example), "  log_level: info", "  log_levl: info", 1), field: "service.log_levl"},
		{name: "zła wartość", config: strings.Replace(string(example), "  port: 8000", "  port: 0", 1), field: "service.port"},
	}
	for i, rc := range cases {
		if rc.config != "" && rc.config == string(example) {
			return fmt.Errorf("przypadek %q: konfiguracja przykładowa zmieniła się i podmiana nie zadziałała", rc.name)
		}
		name := c.containerName(fmt.Sprintf("odmowa-%d", i))
		// Zapisany do sprzątania od razu: po przekroczeniu limitu kontener
		// zostaje, bo przerwany `docker run` go nie zatrzymuje.
		c.containers = append(c.containers, name)
		spec := container.Spec{Name: name, Image: c.Image, NoNetwork: true, Entrypoint: c.Entrypoint}
		if rc.config != "" {
			dir, err := configDir(tmp, fmt.Sprintf("odmowa-%d", i), rc.config)
			if err != nil {
				return err
			}
			spec.Mounts = []container.Mount{{Source: dir, Target: "/app/config"}}
		}
		exit, err := c.Containers.RunToExit(ctx, spec, c.Limits.RefusalTimeout)
		if err != nil {
			return fmt.Errorf("%s: %w", rc.name, err)
		}
		refusal := Refusal{Case: rc.name, ExitCode: exit.Code, Refused: !exit.TimedOut && exit.Code != 0, Output: exit.Output()}
		rep.Refusals = append(rep.Refusals, refusal)
		switch {
		case exit.TimedOut:
			fail("%s: kontener wstał i działał %v zamiast odmówić startu", rc.name, c.Limits.RefusalTimeout)
		case refusal.ExitCode == 0:
			fail("%s: kontener zakończył się kodem 0 zamiast odmówić startu", rc.name)
		case !strings.Contains(refusal.Output, "BŁĄD KONFIGURACJI"):
			fail("%s: odmowa startu bez czytelnego komunikatu: %s", rc.name, strings.TrimSpace(refusal.Output))
		case rc.field != "" && !strings.Contains(refusal.Output, rc.field):
			fail("%s: komunikat nie wskazuje pola %s: %s", rc.name, rc.field, strings.TrimSpace(refusal.Output))
		default:
			cli.Step("%s: czytelna odmowa startu (kod %d)", rc.name, refusal.ExitCode)
		}
	}
	return nil
}

func (c *Checker) checkShutdown(ctx context.Context, tmp string, rep *Report, fail func(string, ...any)) error {
	example, err := os.ReadFile(c.ConfigExample)
	if err != nil {
		return err
	}
	dir, err := configDir(tmp, "zamykanie", string(example))
	if err != nil {
		return err
	}
	// Odniesienie: PID 1, który kończy się od razu po SIGTERM. --init stawia
	// przed procesem docker-init, który przekazuje sygnał dziecku (sleep,
	// bez własnej obsługi kończy się na SIGTERM). Bez --init sleep byłby
	// PID 1 bez handlera i jądro w ogóle nie dostarczyłoby mu sygnału — to
	// dokładnie przypadek „proces bez obsługi SIGTERM".
	//
	// Mierzone są dwie rzeczy. Całkowity czas `docker stop` to literalny
	// warunek Etapu 1, ale zawiera narzut demona Dockera zależny od maszyny.
	// Koszt własny (aplikacja minus odniesienie) to liczba, na którą mamy wpływ.
	s := &Shutdown{MaxOwnSeconds: c.Limits.MaxOwnShutdown.Seconds()}
	for i := range c.Limits.Repeats {
		baseline := container.Spec{Name: c.containerName(fmt.Sprintf("odniesienie-%d", i)), Image: c.Image, Init: true, NoNetwork: true, Entrypoint: []string{"sleep", "300"}}
		seconds, err := c.startAndStop(ctx, baseline, false)
		if err != nil {
			return err
		}
		s.BaselineSeconds = append(s.BaselineSeconds, seconds)
	}
	for i := range c.Limits.Repeats {
		seconds, err := c.startAndStop(ctx, c.app(fmt.Sprintf("aplikacja-%d", i), dir), true)
		if err != nil {
			var notReady *notReadyError
			if errors.As(err, &notReady) {
				fail("%v", err)
				return nil
			}
			return err
		}
		s.TotalSeconds = append(s.TotalSeconds, seconds)
	}
	s.BaselineMedian = median(s.BaselineSeconds)
	s.TotalMedian = median(s.TotalSeconds)
	s.OwnSeconds = s.TotalMedian - s.BaselineMedian
	rep.Shutdown = s

	cli.Step("narzut samego docker stop: %.3f s (mediana z %d)", s.BaselineMedian, c.Limits.Repeats)
	if s.OwnSeconds > s.MaxOwnSeconds {
		fail("aplikacja zamyka się %.3f s ponad narzut, limit %.3f s", s.OwnSeconds, s.MaxOwnSeconds)
	} else {
		cli.Step("koszt własny zamykania: %.3f s (limit %.3f s)", s.OwnSeconds, s.MaxOwnSeconds)
	}
	// Całkowity czas raportowany, ale nie na nim opiera się werdykt: narzut
	// demona na tej maszynie waha się między 0,3 a 0,8 s w kolejnych
	// przebiegach — kryterium oparte na sumie mówiłoby o obciążeniu WSL.
	if total := time.Duration(s.TotalMedian * float64(time.Second)); total > c.Limits.TotalShutdownAdvisory {
		cli.Warn("docker stop razem %.3f s, powyżej orientacyjnego progu %v — z czego %.3f s to narzut Dockera", s.TotalMedian, c.Limits.TotalShutdownAdvisory, s.BaselineMedian)
	} else {
		cli.Step("docker stop razem: %.3f s (mediana z %d)", s.TotalMedian, c.Limits.Repeats)
	}
	return nil
}

func (c *Checker) checkInflight(ctx context.Context, tmp string, rep *Report, fail func(string, ...any)) error {
	example, err := os.ReadFile(c.ConfigExample)
	if err != nil {
		return err
	}
	delayMs := c.Limits.InflightDelay.Milliseconds()
	config := strings.Replace(string(example), "  max_fragments: 8", fmt.Sprintf("  max_fragments: 8\n  stub_delay_ms: %d", delayMs), 1)
	if config == string(example) {
		return errors.New("żądanie w locie: nie udało się dopisać llm.stub_delay_ms do konfiguracji przykładowej")
	}
	dir, err := configDir(tmp, "w-locie", config)
	if err != nil {
		return err
	}
	spec := c.app("w-locie", dir)
	if err := c.Containers.Start(ctx, spec); err != nil {
		return err
	}
	c.containers = append(c.containers, spec.Name)
	port, err := c.waitReady(ctx, spec.Name)
	if err != nil {
		var notReady *notReadyError
		if errors.As(err, &notReady) {
			fail("żądanie w locie: %v", err)
			return nil
		}
		return err
	}

	// Żądanie na nowym połączeniu: keep-alive do zamykanego procesu
	// sprawdzałby klienta, a nie serwer.
	type result struct {
		resp loopback.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.HTTP.Get(ctx, loopback.Request{Port: port, Path: "/search?q=w-locie", Timeout: c.Limits.InflightDelay + c.Limits.StopTimeout + 5*time.Second})
		done <- result{resp, err}
	}()

	// SIGTERM, gdy handler śpi: żądanie jest w toku po stronie serwera.
	// Zegar monotoniczny (time.Since): w WSL zegar ścienny skacze po
	// hibernacji i dawał ujemne czasy trwania.
	if err := poll.Sleep(ctx, c.Limits.InflightDelay/4); err != nil {
		return err
	}
	stopStarted := time.Now()
	if err := c.Containers.Stop(ctx, spec.Name, c.Limits.StopTimeout); err != nil {
		return err
	}
	stopSeconds := time.Since(stopStarted).Seconds()
	r := <-done

	inflight := &Inflight{DelaySeconds: c.Limits.InflightDelay.Seconds(), Status: r.resp.Status, RequestSeconds: r.resp.Elapsed.Seconds(), StopSeconds: stopSeconds}
	if r.err != nil {
		inflight.Error = r.err.Error()
	}
	inflight.Completed = r.err == nil && r.resp.Status == 200
	rep.Inflight = inflight
	if !inflight.Completed {
		fail("żądanie w locie przy SIGTERM nie zostało dokończone (status %d, błąd: %v) — uvicorn nie dał mu dokończyć się w shutdown_grace_seconds", r.resp.Status, r.err)
		return nil
	}
	cli.Step("żądanie w locie przy SIGTERM dokończone: 200 po %.2f s, docker stop %.2f s", r.resp.Elapsed.Seconds(), stopSeconds)
	return nil
}

// notReadyError — aplikacja nie odpowiedziała na /healthz. To wynik
// (obraz nie wstaje z konfiguracją przykładową), a nie awaria narzędzia.
type notReadyError struct{ msg string }

func (e *notReadyError) Error() string { return e.msg }

// app — kontener aplikacji z konfiguracją z katalogu dir i portem 8000 na
// 127.0.0.1 hosta.
func (c *Checker) app(suffix, dir string) container.Spec {
	return container.Spec{
		Name: c.containerName(suffix), Image: c.Image, Entrypoint: c.Entrypoint,
		Mounts: []container.Mount{{Source: dir, Target: "/app/config"}}, Publish: []int{8000},
	}
}

// startAndStop uruchamia kontener, czeka na gotowość (opcjonalnie) i zwraca
// czas `docker stop` w sekundach.
func (c *Checker) startAndStop(ctx context.Context, spec container.Spec, waitHTTP bool) (float64, error) {
	if err := c.Containers.Start(ctx, spec); err != nil {
		return 0, err
	}
	c.containers = append(c.containers, spec.Name)
	if waitHTTP {
		if _, err := c.waitReady(ctx, spec.Name); err != nil {
			return 0, err
		}
	} else if err := poll.Sleep(ctx, 800*time.Millisecond); err != nil {
		// Bez HTTP do sprawdzenia — chwila na start procesu.
		return 0, err
	}
	started := time.Now()
	if err := c.Containers.Stop(ctx, spec.Name, c.Limits.StopTimeout); err != nil {
		return 0, err
	}
	return time.Since(started).Seconds(), nil
}

// waitReady czeka, aż kontener odpowie 200 na /healthz; zwraca port hosta.
// Port przydziela Docker (127.0.0.1::8000) — stały port blokowałby
// równoległe biegi.
func (c *Checker) waitReady(ctx context.Context, name string) (int, error) {
	port, err := c.Containers.HostPort(ctx, name, 8000)
	if err != nil {
		return 0, err
	}
	err = poll.Until(ctx, c.Limits.StartupTimeout, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
		resp, err := c.HTTP.Get(ctx, loopback.Request{Port: port, Path: "/healthz", Timeout: time.Second})
		return err == nil && resp.Status == 200, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return 0, &notReadyError{msg: fmt.Sprintf("kontener %s nie odpowiedział 200 na /healthz w %v", name, c.Limits.StartupTimeout)}
	}
	return port, err
}

func (c *Checker) removeContainers(ctx context.Context) error {
	if err := c.Containers.Remove(ctx, c.containers...); err != nil {
		return err
	}
	c.containers = nil
	return nil
}

func (c *Checker) containerName(suffix string) string {
	return fmt.Sprintf("%s-%s", c.Name, suffix)
}

// configDir zapisuje app.yml w katalogu czytelnym dla użytkownika
// kontenera (UID 10001): katalog 0755, plik 0644 — MkdirTemp tworzy 0700.
func configDir(tmp, name, content string) (string, error) {
	dir := filepath.Join(tmp, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "app.yml"), []byte(content), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
