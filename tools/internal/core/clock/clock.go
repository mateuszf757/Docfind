// Package clock to zgodność zegara ściennego maszyny z czasem serwerów,
// z którymi bramki porównują daty (decyzja 66): ważność certyfikatów,
// tokeny konta usługi z czasem życia, wygasanie wyjątków. Pomiary czasu
// w bramkach idą zegarem monotonicznym i rozjazdu nie odczuwają; daty — tak.
package clock

import (
	"fmt"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
)

// Problem — opis rozjazdu zegara ponad contract.MaxClockSkew; pusty —
// w porządku. skew dodatni — zegar maszyny się spieszy.
func Problem(skew time.Duration) string {
	if skew.Abs() <= contract.MaxClockSkew {
		return ""
	}
	direction := "spieszy się"
	if skew < 0 {
		direction = "spóźnia się"
	}
	return fmt.Sprintf("zegar maszyny %s o %v względem API GitHuba (próg %v) — certyfikaty, tokeny i daty wyjątków oceniłby źle; zsynchronizuj zegar (WSL po hibernacji: `wsl --shutdown` w Windows i ponowne uruchomienie), porównaj `date -u` z `curl -sI https://api.github.com | grep -i ^date`", direction, skew.Abs().Round(time.Second), contract.MaxClockSkew)
}
