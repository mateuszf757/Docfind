package poll

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUntil(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := Until(ctx, time.Second, time.Millisecond, func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil || calls != 3 {
		t.Errorf("spełniony za trzecim razem: %v po %d sprawdzeniach", err, calls)
	}

	boom := errors.New("awaria")
	if err := Until(ctx, time.Second, time.Millisecond, func(context.Context) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("błąd sprawdzenia: %v", err)
	}

	if err := Until(ctx, 5*time.Millisecond, time.Millisecond, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, ErrTimeout) {
		t.Errorf("limit czasu: %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Until(cancelled, time.Minute, time.Minute, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("anulowany kontekst: %v", err)
	}
}
