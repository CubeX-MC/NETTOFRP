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

// MAD 对单个尖峰应保持稳健：大部分样本 20ms，一个 500ms 尖峰，
// MAD 只反映"典型"离散程度，不应被尖峰拉高。
func TestMADResistsSpike(t *testing.T) {
	ds := []time.Duration{
		20 * time.Millisecond,
		20 * time.Millisecond,
		20 * time.Millisecond,
		500 * time.Millisecond, // 尖峰
		20 * time.Millisecond,
		20 * time.Millisecond,
	}
	med := medianDuration(ds)
	if med != 20*time.Millisecond {
		t.Fatalf("中位数应为 20ms，实际 %v", med)
	}
	got := mad(ds, med)
	// 偏差序列：全为 0，中位绝对偏差为 0 → MAD 乘标度后仍为 0。
	if got != 0 {
		t.Fatalf("纯尖峰场景 MAD 应为 0（大多数样本无离散），实际 %v", got)
	}
}

func TestMADScalesWithDispersion(t *testing.T) {
	ds := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
		40 * time.Millisecond,
		50 * time.Millisecond,
	}
	med := medianDuration(ds) // 30ms
	got := mad(ds, med)
	// 偏差：20,10,0,10,20 ms → 中位数 10ms → MAD×1.4826 ≈ 14.826ms
	want := time.Duration(1.4826 * 10 * float64(time.Millisecond))
	if got != want {
		t.Fatalf("期望 MAD ≈ %v，实际 %v", want, got)
	}
}
