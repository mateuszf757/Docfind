// Package poll czeka na warunek: sprawdzenie co interwał do limitu czasu,
// przerywane anulowaniem kontekstu (SIGINT, SIGTERM).
//
// Pętle „sprawdź, odczekaj, sprawdź" były w każdej bramce osobno, każda
// z własnym liczeniem terminu i własną funkcją sleep.
package poll

import (
	"context"
	"errors"
	"time"
)

// ErrTimeout — warunek nie spełnił się w limicie czasu. Wywołujący zamienia
// go na komunikat z kontekstem (co nie wstało i po jakim czasie).
var ErrTimeout = errors.New("upłynął limit czasu")

// Sleep czeka d albo do anulowania kontekstu.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Until sprawdza check od razu, a potem co interval, aż zwróci true,
// zwróci błąd albo minie timeout (ErrTimeout). Błąd z check kończy
// czekanie; check, który ma ponawiać mimo błędu (obiekt jeszcze nie
// istnieje), zwraca (false, nil) i sam zapamiętuje błąd do komunikatu.
func Until(ctx context.Context, timeout, interval time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		if err := Sleep(ctx, interval); err != nil {
			return err
		}
	}
}
