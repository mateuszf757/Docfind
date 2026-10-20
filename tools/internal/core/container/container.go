// Package container to słownik lokalnego silnika kontenerów, którym etapy
// (warunki Etapu 1, baza obrazu) rozmawiają z adapterem Dockera: co
// uruchomić i co z tego wyszło. Flagi `docker run` zna adapter.
package container

import "strings"

// Spec — kontener do uruchomienia.
type Spec struct {
	// Name — nazwa unikalna dla biegu, żeby równoległe biegi nie
	// zatrzymywały sobie kontenerów; pusta — nadaje silnik.
	Name  string
	Image string
	// Entrypoint zastępuje ENTRYPOINT obrazu: pierwszy element to program,
	// reszta — jego argumenty. Pusty: obraz uruchamiany tak, jak
	// w produkcji.
	Entrypoint []string
	// NoNetwork — kontener bez sieci.
	NoNetwork bool
	// Init — docker-init jako PID 1, który przekazuje sygnały dziecku.
	Init bool
	// Mounts — katalogi hosta zamontowane tylko do odczytu.
	Mounts []Mount
	// Publish — porty kontenera wystawione na 127.0.0.1 hosta pod portem
	// przydzielonym przez silnik (decyzja 17): stały port blokowałby
	// równoległe biegi.
	Publish []int
	// Hardening — ustawienia bezpieczeństwa poda; nil — jak obraz domyślnie.
	Hardening *Hardening
}

// Hardening — ustawienia bezpieczeństwa poda z charta w języku silnika
// kontenerów: z nimi dym obrazu uruchamia kontener tak, jak uruchamia go
// klaster (decyzja 72).
type Hardening struct {
	// ReadOnlyRoot — readOnlyRootFilesystem: zapis tylko do wolumenów.
	ReadOnlyRoot bool
	// Tmpfs — ścieżki wolumenów emptyDir: jedyne miejsca zapisu.
	Tmpfs []string
	// CapDropAll — capabilities.drop: [ALL].
	CapDropAll bool
	// NoNewPrivileges — allowPrivilegeEscalation: false.
	NoNewPrivileges bool
	// User — runAsUser[:runAsGroup]; puste — użytkownik z obrazu.
	User string
}

// String — ustawienia do komunikatów i raportów.
func (h Hardening) String() string {
	var parts []string
	if h.ReadOnlyRoot {
		parts = append(parts, "system plików tylko do odczytu")
	}
	for _, p := range h.Tmpfs {
		parts = append(parts, "zapis w "+p)
	}
	if h.CapDropAll {
		parts = append(parts, "bez capabilities")
	}
	if h.NoNewPrivileges {
		parts = append(parts, "bez eskalacji uprawnień")
	}
	if h.User != "" {
		parts = append(parts, "użytkownik "+h.User)
	}
	if len(parts) == 0 {
		return "bez ustawień poda"
	}
	return strings.Join(parts, ", ")
}

// Mount — katalog hosta zamontowany tylko do odczytu.
type Mount struct {
	Source string
	Target string
}

// Exit — kontener uruchomiony do końca.
type Exit struct {
	// Code — kod wyjścia procesu w kontenerze.
	Code int
	// TimedOut — kontener działał dłużej niż limit i został usunięty.
	TimedOut bool
	Stdout   string
	Stderr   string
}

// Output — stdout i stderr razem, do komunikatów i raportu.
func (e Exit) Output() string { return e.Stdout + e.Stderr }
