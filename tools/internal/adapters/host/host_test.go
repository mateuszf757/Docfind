package host

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPortFree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if free, err := (Host{}).PortFree(port); free || err != nil {
		t.Errorf("zajęty port: %v, %v", free, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if free, err := (Host{}).PortFree(port); !free || err != nil {
		t.Errorf("zwolniony port: %v, %v", free, err)
	}
}

func TestInotify(t *testing.T) {
	limit, err := (Host{}).InotifyLimit()
	if err != nil || limit <= 0 {
		t.Errorf("limit inotify: %d, %v", limit, err)
	}
	if used := (Host{}).InotifyUsed(); used < 0 {
		t.Errorf("zużycie: %d", used)
	}
}

func TestClockSkew(t *testing.T) {
	ahead := time.Now().Add(-5 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", ahead.UTC().Format(http.TimeFormat))
	}))
	defer srv.Close()
	skew, err := Host{}.ClockSkew(context.Background(), srv.URL)
	// Serwer jest 5 minut „w tyle”, więc zegar maszyny spieszy się o ~5 min.
	if err != nil || skew < 4*time.Minute+58*time.Second || skew > 5*time.Minute+2*time.Second {
		t.Errorf("ClockSkew = %v, %v", skew, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", "wczoraj")
	}))
	defer bad.Close()
	if _, err := (Host{}).ClockSkew(context.Background(), bad.URL); err == nil {
		t.Error("nieczytelny nagłówek Date przyjęty")
	}
}
