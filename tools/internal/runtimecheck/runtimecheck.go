// Package runtimecheck to warunki zakończenia Etapu 1 sprawdzane na
// zbudowanym obrazie (dawniej ci/check-runtime.sh), rozszerzone o złą
// konfigurację (U11b) i żądanie w locie przy SIGTERM (U3).
package runtimecheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

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
	MaxImageUnpackedBytes: 200_000_000,
	MaxOwnShutdown:        750 * time.Millisecond,
	TotalShutdownAdvisory: time.Second,
	StopTimeout:           10 * time.Second,
	StartupTimeout:        20 * time.Second,
	RefusalTimeout:        30 * time.Second,
	Repeats:               5,
	InflightDelay:         1500 * time.Millisecond,
}

// Checker sprawdza jeden obraz.
type Checker struct {
	Docker docker.Client
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
	size, err := c.Docker.ImageSize(ctx, c.Image)
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
		args := []string{"run", "--rm", "--network", "none", "--name", name}
		if rc.config != "" {
			dir, err := configDir(tmp, fmt.Sprintf("odmowa-%d", i), rc.config)
			if err != nil {
				return err
			}
			args = append(args, "-v", dir+":/app/config:ro")
		}
		args = append(args, c.entrypointArgs()...)
		args = append(args, c.Image)
		args = append(args, c.entrypointRest()...)

		refusal, err := c.runRefusal(ctx, rc, name, args)
		if err != nil {
			return err
		}
		rep.Refusals = append(rep.Refusals, refusal)
		switch {
		case !refusal.Refused && refusal.ExitCode == -1:
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

// runRefusal uruchamia kontener, który ma odmówić startu, z limitem czasu.
//
// Po limicie usuwany jest sam kontener, a nie przerywany `docker run`: CLI
// przekazuje sygnał do kontenera, a PID 1 bez handlera go ignoruje, więc
// przerwany klient czekałby na swój własny limit. Usunięcie kontenera
// kończy `docker run` od razu.
func (c *Checker) runRefusal(ctx context.Context, rc refusalCase, name string, args []string) (Refusal, error) {
	var timedOut atomic.Bool
	timer := time.AfterFunc(c.Limits.RefusalTimeout, func() {
		timedOut.Store(true)
		cleanupCtx, cancel := cli.CleanupContext(30 * time.Second)
		defer cancel()
		_, _ = c.Docker.Run(cleanupCtx, "rm", "--force", name)
	})
	res, err := c.Docker.Run(ctx, args...)
	timer.Stop()
	output := string(res.Stdout) + string(res.Stderr)
	refusal := Refusal{Case: rc.name, Output: output}
	switch {
	case timedOut.Load():
		refusal.ExitCode = -1
	case err == nil:
		refusal.ExitCode = 0
	case proc.ExitCode(err) > 0:
		refusal.ExitCode = proc.ExitCode(err)
		refusal.Refused = true
	default:
		return refusal, fmt.Errorf("%s: %w", rc.name, err)
	}
	return refusal, nil
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
		name := c.containerName(fmt.Sprintf("odniesienie-%d", i))
		seconds, err := c.startAndStop(ctx, name, []string{"--init", "--network", "none", "--entrypoint", "sleep", c.Image, "300"}, false)
		if err != nil {
			return err
		}
		s.BaselineSeconds = append(s.BaselineSeconds, seconds)
	}
	for i := range c.Limits.Repeats {
		name := c.containerName(fmt.Sprintf("aplikacja-%d", i))
		args := append([]string{"-p", "127.0.0.1::8000", "-v", dir + ":/app/config:ro"}, c.entrypointArgs()...)
		args = append(args, c.Image)
		args = append(args, c.entrypointRest()...)
		seconds, err := c.startAndStop(ctx, name, args, true)
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
	name := c.containerName("w-locie")
	args := append([]string{"run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8000", "-v", dir + ":/app/config:ro"}, c.entrypointArgs()...)
	args = append(args, c.Image)
	args = append(args, c.entrypointRest()...)
	if _, err := c.Docker.Run(ctx, args...); err != nil {
		return fmt.Errorf("start kontenera %s: %w", name, err)
	}
	c.containers = append(c.containers, name)
	base, err := c.waitReady(ctx, name)
	if err != nil {
		var notReady *notReadyError
		if errors.As(err, &notReady) {
			fail("żądanie w locie: %v", err)
			return nil
		}
		return err
	}

	// Każde żądanie na nowym połączeniu: keep-alive do zamykanego procesu
	// sprawdzałby klienta, a nie serwer.
	client := &http.Client{Timeout: c.Limits.InflightDelay + c.Limits.StopTimeout + 5*time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	type result struct {
		status  int
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	started := time.Now()
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/search?q=w-locie", nil)
		if err != nil {
			done <- result{err: err}
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err, elapsed: time.Since(started)}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		done <- result{status: resp.StatusCode, elapsed: time.Since(started)}
	}()

	// SIGTERM, gdy handler śpi: żądanie jest w toku po stronie serwera.
	// Zegar monotoniczny (time.Since): w WSL zegar ścienny skacze po
	// hibernacji i dawał ujemne czasy trwania.
	select {
	case <-time.After(c.Limits.InflightDelay / 4):
	case <-ctx.Done():
		return ctx.Err()
	}
	stopStarted := time.Now()
	if _, err := c.Docker.Run(ctx, "stop", "--timeout", seconds(c.Limits.StopTimeout), name); err != nil {
		return fmt.Errorf("docker stop %s: %w", name, err)
	}
	stopSeconds := time.Since(stopStarted).Seconds()
	r := <-done

	inflight := &Inflight{DelaySeconds: c.Limits.InflightDelay.Seconds(), Status: r.status, RequestSeconds: r.elapsed.Seconds(), StopSeconds: stopSeconds}
	if r.err != nil {
		inflight.Error = r.err.Error()
	}
	inflight.Completed = r.status == http.StatusOK
	rep.Inflight = inflight
	if r.err != nil || r.status != http.StatusOK {
		fail("żądanie w locie przy SIGTERM nie zostało dokończone (status %d, błąd: %v) — uvicorn nie dał mu dokończyć się w shutdown_grace_seconds", r.status, r.err)
		return nil
	}
	cli.Step("żądanie w locie przy SIGTERM dokończone: 200 po %.2f s, docker stop %.2f s", r.elapsed.Seconds(), stopSeconds)
	return nil
}

// notReadyError — aplikacja nie odpowiedziała na /healthz. To wynik
// (obraz nie wstaje z konfiguracją przykładową), a nie awaria narzędzia.
type notReadyError struct{ msg string }

func (e *notReadyError) Error() string { return e.msg }

// startAndStop uruchamia kontener, czeka na gotowość (opcjonalnie) i zwraca
// czas `docker stop` w sekundach.
func (c *Checker) startAndStop(ctx context.Context, name string, args []string, waitHTTP bool) (float64, error) {
	runArgs := append([]string{"run", "-d", "--rm", "--name", name}, args...)
	if _, err := c.Docker.Run(ctx, runArgs...); err != nil {
		return 0, fmt.Errorf("start kontenera %s: %w", name, err)
	}
	c.containers = append(c.containers, name)
	if waitHTTP {
		if _, err := c.waitReady(ctx, name); err != nil {
			return 0, err
		}
	} else {
		// Bez HTTP do sprawdzenia — chwila na start procesu, jak wcześniej.
		select {
		case <-time.After(800 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	started := time.Now()
	if _, err := c.Docker.Run(ctx, "stop", "--timeout", seconds(c.Limits.StopTimeout), name); err != nil {
		return 0, fmt.Errorf("docker stop %s: %w", name, err)
	}
	return time.Since(started).Seconds(), nil
}

// waitReady czeka, aż kontener odpowie 200 na /healthz; zwraca adres bazowy.
// Port hosta przydziela Docker (127.0.0.1::8000) — stały port blokowałby
// równoległe biegi i wiązał się z każdym interfejsem, gdyby zabrakło adresu.
func (c *Checker) waitReady(ctx context.Context, name string) (string, error) {
	out, err := c.Docker.Run(ctx, "port", name, "8000/tcp")
	if err != nil {
		return "", fmt.Errorf("docker port %s: %w", name, err)
	}
	hostPort := ""
	for _, line := range strings.Split(strings.TrimSpace(string(out.Stdout)), "\n") {
		if host, port, err := net.SplitHostPort(strings.TrimSpace(line)); err == nil && host == "127.0.0.1" {
			hostPort = net.JoinHostPort(host, port)
			break
		}
	}
	if hostPort == "" {
		return "", fmt.Errorf("docker port %s: brak portu na 127.0.0.1 w %q", name, out.Stdout)
	}
	base := "http://" + hostPort
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(c.Limits.StartupTimeout)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return "", err
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, nil
			}
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", &notReadyError{msg: fmt.Sprintf("kontener %s nie odpowiedział 200 na /healthz w %v", name, c.Limits.StartupTimeout)}
}

func (c *Checker) removeContainers(ctx context.Context) error {
	if len(c.containers) == 0 {
		return nil
	}
	args := append([]string{"rm", "--force"}, c.containers...)
	res, err := c.Docker.Run(ctx, args...)
	// `docker rm -f` na kontenerze już usuniętym (--rm) zgłasza „No such
	// container" — to nie błąd sprzątania.
	if err != nil && !bytes.Contains(res.Stderr, []byte("No such container")) {
		return fmt.Errorf("sprzątanie kontenerów: %w", err)
	}
	c.containers = nil
	return nil
}

func (c *Checker) containerName(suffix string) string {
	return fmt.Sprintf("%s-%s", c.Name, suffix)
}

// entrypointArgs i entrypointRest rozkładają nadpisany ENTRYPOINT na flagę
// (pierwszy element) i argumenty po nazwie obrazu.
func (c *Checker) entrypointArgs() []string {
	if len(c.Entrypoint) == 0 {
		return nil
	}
	return []string{"--entrypoint", c.Entrypoint[0]}
}

func (c *Checker) entrypointRest() []string {
	if len(c.Entrypoint) <= 1 {
		return nil
	}
	return c.Entrypoint[1:]
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

func seconds(d time.Duration) string {
	return fmt.Sprintf("%d", int(d.Round(time.Second)/time.Second))
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
