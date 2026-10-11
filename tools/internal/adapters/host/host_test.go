package host

import (
	"net"
	"testing"
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
