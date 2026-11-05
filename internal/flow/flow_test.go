package flow

import (
	"context"
	"testing"
	"time"
)

// A sender that cannot keep up is always behind schedule; the pacer must
// still stop at the deadline (regression: endless loop on a slow VPS).
func TestPacerStopsWhenBehind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	p := NewPacer(1e9) // impossible rate: always behind
	done := make(chan struct{})
	go func() {
		for p.Next(ctx, 64) > 0 {
			time.Sleep(time.Millisecond) // a slow sender
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pacer ignored the deadline while behind schedule")
	}
}

func TestPacerRate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	p := NewPacer(2000)
	n := 0
	for {
		k := p.Next(ctx, 64)
		if k == 0 {
			break
		}
		n += k
	}
	if n < 850 || n > 1100 {
		t.Errorf("2000 pps for 0.5 s released %d packets", n)
	}
}
