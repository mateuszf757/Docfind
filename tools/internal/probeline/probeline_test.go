package probeline

import (
	"strings"
	"testing"
)

func TestParseAndSummarize(t *testing.T) {
	log := strings.Join([]string{
		`{"t":"2026-10-07T08:00:00Z","seq":1,"code":200,"dns_ms":1,"connect_ms":0.5,"total_ms":3,"connected":true}`,
		`{"t":"2026-10-07T08:00:01Z","seq":2,"code":0,"error":"dial tcp: lookup docfind-api: no such host","dns_ms":2000,"total_ms":2000,"connected":false}`,
		`{"t":"2026-10-07T08:00:02Z","seq":3,"code":503,"connected":true}`,
		``,
	}, "\n")
	results, err := Parse([]byte(log))
	if err != nil {
		t.Fatal(err)
	}
	s := Summarize(results)
	if s.Total != 3 || s.Failed != 2 || s.NotConnected != 1 || len(s.Failures) != 2 {
		t.Errorf("podsumowanie %+v", s)
	}
	// Linia, która nie jest wynikiem, to awaria sondy, a nie nieudane żądanie.
	if _, err := Parse([]byte("BŁĄD: zapis wyniku\n")); err == nil {
		t.Error("nie-wynik przyjęty jako wynik")
	}
}
