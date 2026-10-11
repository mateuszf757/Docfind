// Package cli zbiera konwencje wspólne dla programów w tools/: kody wyjścia,
// postać komunikatów i reakcję na sygnały. Te same konwencje miały skrypty
// w ci/ — bramka, która raz kończy się kodem 1 przy awarii narzędzia, a raz
// przy niespełnionym warunku, uczy ignorowania czerwonego wyniku.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Kody wyjścia wszystkich bramek w repozytorium.
const (
	// ExitOK — wynik pozytywny.
	ExitOK = 0
	// ExitUnmet — wynik negatywny: bramka zadziałała i warunek nie jest spełniony.
	ExitUnmet = 1
	// ExitFailure — awaria narzędzia: brak programu, sieci, nieczytelne dane.
	// Wynik jest nieznany, a nie negatywny.
	ExitFailure = 2
)

// Out i Err to miejsca, do których trafiają komunikaty. Zmienne, żeby testy
// mogły podstawić bufor.
var (
	Out io.Writer = os.Stdout
	Err io.Writer = os.Stderr
)

// UnmetError oznacza niespełniony warunek — wynik bramki, a nie jej awarię.
type UnmetError struct {
	Msg string
	// Reported mówi, że szczegóły zostały już wypisane (Verdict) i Report
	// wypisuje tylko podsumowanie.
	Reported bool
}

func (e *UnmetError) Error() string { return e.Msg }

// Unmet tworzy błąd „warunek niespełniony" (kod wyjścia 1).
func Unmet(format string, args ...any) error {
	return &UnmetError{Msg: fmt.Sprintf(format, args...)}
}

// UsageError to złe wywołanie programu — awaria po stronie wołającego.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// Usage tworzy błąd złego wywołania (kod wyjścia 2).
func Usage(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// ExitCode zamienia wynik polecenia na kod wyjścia. Wszystko, co nie jest
// jawnie wynikiem negatywnym, jest awarią: błąd, którego nikt nie
// sklasyfikował, nie może udawać werdyktu.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var unmet *UnmetError
	if errors.As(err, &unmet) {
		return ExitUnmet
	}
	return ExitFailure
}

// Report wypisuje błąd w konwencji repozytorium: NIESPEŁNIONE dla wyniku
// negatywnego, BŁĄD dla awarii.
func Report(err error) {
	if err == nil {
		return
	}
	var unmet *UnmetError
	if errors.As(err, &unmet) {
		fmt.Fprintf(Err, "NIESPEŁNIONE: %s\n", unmet.Msg)
		return
	}
	fmt.Fprintf(Err, "BŁĄD: %v\n", err)
}

// Step wypisuje krok postępu: „==> …" na stdout.
func Step(format string, args ...any) {
	fmt.Fprintf(Out, "==> %s\n", fmt.Sprintf(format, args...))
}

// Warn wypisuje ostrzeżenie, które nie zmienia wyniku: „UWAGA: …" na stderr.
func Warn(format string, args ...any) {
	fmt.Fprintf(Err, "UWAGA: %s\n", fmt.Sprintf(format, args...))
}

// Verdict zbiera niespełnione warunki bramki, która sprawdza kilka rzeczy
// i zgłasza wszystkie naraz. Zatrzymanie na pierwszym ukryłoby kolejne,
// a każdy przebieg kosztuje minutę budowania obrazu albo klaster.
type Verdict struct {
	failures []string
}

// Fail wypisuje niespełniony warunek od razu i zapamiętuje go.
func (v *Verdict) Fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(Err, "NIESPEŁNIONE: %s\n", msg)
	v.failures = append(v.failures, msg)
}

// Failed mówi, czy któryś warunek nie został spełniony.
func (v *Verdict) Failed() bool { return len(v.failures) > 0 }

// Err zwraca nil, gdy wszystko spełnione, albo podsumowanie jako UnmetError.
func (v *Verdict) Err(what string) error {
	if len(v.failures) == 0 {
		return nil
	}
	return &UnmetError{Msg: fmt.Sprintf("%s: %d niespełnionych", what, len(v.failures)), Reported: true}
}

// SignalContext zwraca kontekst anulowany przy SIGINT i SIGTERM.
//
// Dlaczego to ważne: bramki zmieniają stan poza procesem — cordon węzła,
// sonda w klastrze, kontenery testowe. Domyślnie Go kończy proces na
// SIGINT natychmiast, bez wykonania odroczonych funkcji (defer), więc
// sprzątanie by nie zaszło. Z NotifyContext sygnał tylko anuluje kontekst:
// bieżące operacje dostają ctx.Done(), programy zewnętrzne uruchomione przez
// proc dostają SIGTERM, a kod wraca normalną ścieżką przez defer. Sprzątanie
// musi wtedy użyć nowego kontekstu (CleanupContext), bo ten jest już
// anulowany.
//
// Po pierwszym sygnale obsługa wraca do domyślnej (stop), więc drugi Ctrl+C
// kończy proces od razu — wyjście awaryjne, gdy sprzątanie samo się zawiesi.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

// CleanupContext zwraca kontekst do sprzątania stanu zewnętrznego — także
// wtedy, gdy główny kontekst został anulowany sygnałem. Niezależny od
// sygnałów, ale z limitem czasu: zawieszone API nie może trzymać procesu
// bez końca.
func CleanupContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
