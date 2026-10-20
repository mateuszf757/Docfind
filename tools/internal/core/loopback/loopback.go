// Package loopback to słownik żądań HTTP(S) do usług wystawionych na
// 127.0.0.1 hosta (decyzja 17): kontenera z warunków Etapu 1 i load
// balancera klastra k3d. Adapter łączy się wyłącznie z 127.0.0.1 — nazwa
// hosta trafia do SNI i nagłówka Host, jak `curl --resolve`.
package loopback

import (
	"fmt"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Request — jedno żądanie GET. Każde idzie nowym połączeniem (dla HTTPS —
// nowym handshake'iem, czyli dokładnie tym momentem, w którym serwer podaje
// certyfikat) i bez podążania za przekierowaniem.
type Request struct {
	Port int
	// Host — nazwa w SNI i nagłówku Host; pusta — 127.0.0.1.
	Host string
	Path string
	// TLS — HTTPS z tym zaufaniem; nil — zwykłe HTTP.
	TLS     *Trust
	Timeout time.Duration
	// Bearer — token w nagłówku Authorization; pusty — bez nagłówka.
	Bearer sensitive.Value
	// ReadBody — ile bajtów odpowiedzi zachować w Response.Body; 0 — treść
	// odrzucana (pętle sond nie buforują odpowiedzi).
	ReadBody int64
}

// URL — adres żądania, do komunikatów i nagłówka Location.
func (r Request) URL() string {
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, r.Port, r.Path)
}

// Trust — względem czego weryfikować certyfikat serwera.
type Trust struct {
	// RootCAs — certyfikaty głównych CA w PEM; puste — systemowy magazyn.
	RootCAs []byte
	// SkipVerify — bez weryfikacji łańcucha. Tylko dla Let's Encrypt
	// staging, którego CA z założenia nie ma w żadnym magazynie; wystawcę
	// sprawdza wtedy wywołujący na podanym certyfikacie.
	SkipVerify bool
}

// Response — odpowiedź.
type Response struct {
	Status int
	// Header — pierwsze wartości nagłówków, klucze w postaci kanonicznej
	// (X-Request-Id, Location).
	Header map[string]string
	// Elapsed — czas od wysłania do przeczytania całej odpowiedzi (zegar
	// monotoniczny).
	Elapsed time.Duration
	// Body — początek treści, gdy Request.ReadBody > 0.
	Body []byte
}
