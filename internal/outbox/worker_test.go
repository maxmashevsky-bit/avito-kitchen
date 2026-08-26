package outbox

import (
	"testing"
	"time"
)

func TestBackoffIsBoundedExponential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{{0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {5, 16 * time.Second}, {20, 32 * time.Second}}
	for _, test := range tests {
		if got := Backoff(test.attempt); got != test.want {
			t.Errorf("Backoff(%d) = %s, want %s", test.attempt, got, test.want)
		}
	}
}
