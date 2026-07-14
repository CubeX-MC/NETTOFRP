package prober

import (
	"testing"
	"time"
)

func TestMinDuration(t *testing.T) {
	got := minDuration([]time.Duration{
		35 * time.Millisecond,
		12 * time.Millisecond,
		28 * time.Millisecond,
	})
	if got != 12*time.Millisecond {
		t.Fatalf("期望最小延迟 12ms，实际 %v", got)
	}
}
