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

func TestMedianDurationOdd(t *testing.T) {
	got := medianDuration([]time.Duration{
		35 * time.Millisecond,
		12 * time.Millisecond,
		28 * time.Millisecond,
	})
	if got != 28*time.Millisecond {
		t.Fatalf("奇数样本中位数应为 28ms，实际 %v", got)
	}
}

func TestMedianDurationEven(t *testing.T) {
	got := medianDuration([]time.Duration{
		10 * time.Millisecond,
		40 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	})
	if got != 25*time.Millisecond {
		t.Fatalf("偶数样本中位数应为 25ms（20 与 30 平均），实际 %v", got)
	}
}

func TestMedianDurationDoesNotMutateInput(t *testing.T) {
	input := []time.Duration{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	medianDuration(input)
	if input[0] != 30*time.Millisecond || input[1] != 10*time.Millisecond || input[2] != 20*time.Millisecond {
		t.Fatalf("中位数计算不应修改输入切片，实际 %v", input)
	}
}
