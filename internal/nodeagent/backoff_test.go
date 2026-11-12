package nodeagent

import (
	"testing"
	"time"
)

// TestBackoffJitterBounds pins the jitter envelope and the exponential cap.
func TestBackoffJitterBounds(t *testing.T) {
	min, max := time.Second, 60*time.Second
	for attempt := 0; attempt < 12; attempt++ {
		base := min
		for i := 0; i < attempt; i++ {
			if base >= max/2 {
				base = max
				break
			}
			base *= 2
		}
		if base > max {
			base = max
		}
		ceiling := base + base/2
		if ceiling > max {
			ceiling = max
		}
		for _, f := range []float64{0, 0.25, 0.5, 0.75, 1} {
			delay := BackoffDelay(attempt, min, max, func() float64 { return f })
			if delay < base || delay > ceiling {
				t.Fatalf("attempt %d f=%v: delay %v outside [%v,%v]", attempt, f, delay, base, ceiling)
			}
			if delay < min || delay > max {
				t.Fatalf("attempt %d f=%v: delay %v outside [%v,%v]", attempt, f, delay, min, max)
			}
		}
	}
	if got := BackoffDelay(0, min, max, func() float64 { return 0 }); got != min {
		t.Fatalf("attempt 0 without jitter = %v, want %v", got, min)
	}
	if got := BackoffDelay(0, min, max, func() float64 { return 1 }); got != min+min/2 {
		t.Fatalf("attempt 0 with full jitter = %v, want %v", got, min+min/2)
	}
	if got := BackoffDelay(20, min, max, func() float64 { return 1 }); got != max {
		t.Fatalf("attempt 20 with full jitter = %v, want %v", got, max)
	}
	// Nil entropy must still stay in bounds.
	for i := 0; i < 100; i++ {
		if d := BackoffDelay(3, min, max, nil); d < min || d > max {
			t.Fatalf("default entropy produced %v", d)
		}
	}
}
