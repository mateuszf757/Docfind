// Package container to słownik lokalnego silnika kontenerów, którym etapy
// (warunki Etapu 1, baza obrazu) rozmawiają z adapterem Dockera: co
// uruchomić i co z tego wyszło. Flagi `docker run` zna adapter.
package container

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
