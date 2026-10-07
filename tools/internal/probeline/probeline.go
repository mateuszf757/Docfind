// Package probeline to format wyników sondy drainu: jedna linia JSON na
// żądanie. Wspólny dla sondy (pisze) i bramki (czyta), tylko biblioteka
// standardowa — sonda jedzie do obrazu bez systemu bazowego.
package probeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Result — wynik jednego żądania sondy.
type Result struct {
	Time      time.Time `json:"t"`
	Seq       int       `json:"seq"`
	Code      int       `json:"code"`
	Error     string    `json:"error,omitempty"`
	DNSMs     float64   `json:"dns_ms"`
	ConnectMs float64   `json:"connect_ms"`
	TotalMs   float64   `json:"total_ms"`
	// Connected — czy doszło do połączenia TCP. Bez połączenia przyczyna
	// leży w DNS albo braku trasy (pusty endpoint), a nie w API.
	Connected bool `json:"connected"`
}

// OK — żądanie zakończone odpowiedzią 200.
func (r Result) OK() bool { return r.Error == "" && r.Code == http.StatusOK }

// Parse czyta log sondy. Linia, która nie jest wynikiem, to awaria sondy
// (np. komunikat BŁĄD na stderr), a nie nieudane żądanie.
func Parse(log []byte) ([]Result, error) {
	var results []Result
	scanner := bufio.NewScanner(bytes.NewReader(log))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal(text, &r); err != nil {
			return nil, fmt.Errorf("log sondy, linia %d: %q nie jest wynikiem żądania: %w", line, truncate(string(text), 200), err)
		}
		results = append(results, r)
	}
	return results, scanner.Err()
}

// Summary — podsumowanie logu sondy.
type Summary struct {
	Total  int `json:"total"`
	Failed int `json:"failed"`
	// NotConnected — nieudane bez połączenia TCP: DNS albo brak trasy.
	NotConnected int `json:"not_connected"`
	// Failures — pierwsze nieudane żądania, do diagnozy.
	Failures []Result `json:"failures,omitempty"`
}

// MaxListedFailures — ile nieudanych żądań trafia do raportu.
const MaxListedFailures = 20

// Summarize liczy żądania i nieudane.
func Summarize(results []Result) Summary {
	s := Summary{Total: len(results)}
	for _, r := range results {
		if r.OK() {
			continue
		}
		s.Failed++
		if !r.Connected {
			s.NotConnected++
		}
		if len(s.Failures) < MaxListedFailures {
			s.Failures = append(s.Failures, r)
		}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
