// Package httpclient to adapter żądań HTTP(S) do usług wystawionych na
// 127.0.0.1 hosta (core/loopback): kontenera z warunków Etapu 1 i load
// balancera klastra k3d. Połączenie idzie zawsze na 127.0.0.1 — adapter
// nie potrafi trafić nigdzie indziej — a nazwa hosta tylko do SNI
// i nagłówka Host, jak `curl --resolve`.
package httpclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/loopback"
)

// Client — klient żądań na loopbacku.
type Client struct{}

func tlsConfig(r loopback.Request) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: r.Host, MinVersion: tls.VersionTLS12}
	switch {
	case r.TLS.SkipVerify:
		cfg.InsecureSkipVerify = true
	case len(r.TLS.RootCAs) > 0:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(r.TLS.RootCAs) {
			return nil, errors.New("nieczytelne certyfikaty głównych CA")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func dialer(r loopback.Request) *net.Dialer {
	return &net.Dialer{Timeout: r.Timeout}
}

func address(r loopback.Request) string { return fmt.Sprintf("127.0.0.1:%d", r.Port) }

// Get wysyła żądanie nowym połączeniem (bez keep-alive: pętla na jednym
// połączeniu testowałaby stary handshake albo umierający proces) i czyta
// całą odpowiedź. Przekierowanie jest odpowiedzią, nie krokiem.
func (Client) Get(ctx context.Context, r loopback.Request) (loopback.Response, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer(r).DialContext(ctx, network, address(r))
		},
	}
	if r.TLS != nil {
		cfg, err := tlsConfig(r)
		if err != nil {
			return loopback.Response{}, err
		}
		transport.TLSClientConfig = cfg
	}
	client := &http.Client{
		Timeout:       r.Timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL(), nil)
	if err != nil {
		return loopback.Response{}, err
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return loopback.Response{Elapsed: time.Since(started)}, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		return loopback.Response{}, err
	}
	header := map[string]string{}
	for k := range resp.Header {
		header[k] = resp.Header.Get(k)
	}
	return loopback.Response{Status: resp.StatusCode, Header: header, Elapsed: time.Since(started)}, nil
}

// Certificate zwraca certyfikat, który serwer podaje w handshake'u z tą
// nazwą w SNI.
func (Client) Certificate(ctx context.Context, r loopback.Request) (*x509.Certificate, error) {
	if r.TLS == nil {
		return nil, errors.New("certyfikat tylko przez TLS")
	}
	cfg, err := tlsConfig(r)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{NetDialer: dialer(r), Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", address(r))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("serwer nie podał certyfikatu")
	}
	return certs[0], nil
}
