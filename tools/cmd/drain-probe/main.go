// Program drain-probe — sonda bramki drainu, uruchamiana jako pod w klastrze.
//
// Wysyła żądania do Service w stałym tempie i wypisuje wynik każdego jako
// linię JSON; werdykt wydaje dft po przeczytaniu logu poda. Każde żądanie
// idzie nowym połączeniem i z nowym rozwiązaniem nazwy:
//
//   - nowe połączenie (DisableKeepAlives) — klient trzymający keep-alive do
//     umierającego poda testowałby klienta, a nie Service i endpointy;
//   - nowe rozwiązanie nazwy — resolver Go nie ma pamięci podręcznej, więc
//     każde połączenie pyta CoreDNS. Jeden długo żyjący curl trzymałby wynik
//     60 s (libcurl) i przeoczył awarię DNS przy drainie węzła z CoreDNS —
//     dokładnie ten błąd, który znalazła pierwsza wersja bramki (decyzja 18).
//
// Tylko biblioteka standardowa: program jedzie do obrazu bez systemu
// bazowego, a każda zależność to kod w klastrze.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/probeline"
)

func main() {
	url := flag.String("url", "", "adres żądań, np. http://docfind-api.docfind.svc.cluster.local/search?q=drain")
	interval := flag.Duration("interval", 50*time.Millisecond, "przerwa między żądaniami")
	timeout := flag.Duration("timeout", 2*time.Second, "limit czasu jednego żądania")
	flag.Parse()
	if *url == "" {
		fmt.Fprintln(os.Stderr, "BŁĄD: podaj -url")
		os.Exit(2)
	}

	// SIGTERM od kubeleta przy usuwaniu poda kończy pętlę normalnie, a nie
	// w połowie zapisu linii.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{Timeout: *timeout}).DialContext,
		},
	}
	out := json.NewEncoder(os.Stdout)
	for seq := 1; ctx.Err() == nil; seq++ {
		r := probe(ctx, client, *url)
		r.Seq = seq
		if ctx.Err() != nil {
			return // żądanie przerwane zakończeniem sondy, a nie przez drain
		}
		if err := out.Encode(r); err != nil {
			fmt.Fprintf(os.Stderr, "BŁĄD: zapis wyniku: %v\n", err)
			os.Exit(2)
		}
		select {
		case <-ctx.Done():
		case <-time.After(*interval):
		}
	}
}

// probe wysyła jedno żądanie i mierzy fazy zegarem monotonicznym
// (time.Since): w WSL zegar ścienny potrafi skoczyć po hibernacji.
func probe(ctx context.Context, client *http.Client, url string) probeline.Result {
	r := probeline.Result{Time: time.Now().UTC()}
	started := time.Now()
	var dnsStart, connectStart time.Time
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				r.DNSMs = ms(time.Since(dnsStart))
			}
		},
		ConnectStart: func(string, string) { connectStart = time.Now() },
		ConnectDone: func(_, _ string, err error) {
			if err == nil && !connectStart.IsZero() {
				r.Connected = true
				r.ConnectMs = ms(time.Since(connectStart))
			}
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	resp, err := client.Do(req)
	if err != nil {
		r.Error = err.Error()
		r.TotalMs = ms(time.Since(started))
		return r
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	r.Code = resp.StatusCode
	r.TotalMs = ms(time.Since(started))
	return r
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
