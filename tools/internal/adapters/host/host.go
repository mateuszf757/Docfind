// Package host to adapter maszyny, na której stoi klaster lokalny: limity
// jądra i zajętość portów na 127.0.0.1. Ocena (ile wystarczy, co zrobić)
// należy do etapu wdrożenia.
package host

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Host — bieżąca maszyna.
type Host struct{}

// InotifyLimit zwraca fs.inotify.max_user_instances.
func (Host) InotifyLimit() (int, error) {
	raw, err := os.ReadFile("/proc/sys/fs/inotify/max_user_instances")
	if err != nil {
		return 0, fmt.Errorf("limit inotify: %w", err)
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("nieczytelny limit inotify %q", raw)
	}
	return limit, nil
}

// InotifyUsed liczy instancje inotify z deskryptorów procesów użytkownika
// (anon_inode:inotify) — cudzych procesów i tak nie da się odczytać.
func (Host) InotifyUsed() int {
	used := 0
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if target, err := os.Readlink(fd); err == nil && target == "anon_inode:inotify" {
			used++
		}
	}
	return used
}

// PortFree mówi, czy port na 127.0.0.1 jest wolny: próba związania
// i natychmiastowe zwolnienie — rozstrzyga ten sam mechanizm, który potem
// zawiódłby k3d. Zajęty port to (false, nil); błąd — inna przyczyna, przez
// którą nie da się go związać.
func (Host) PortFree(port int) (bool, error) {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if errors.Is(err, syscall.EADDRINUSE) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, l.Close()
}
