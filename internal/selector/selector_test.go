package selector

import (
	"testing"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/prober"
)

func newSel() *Selector {
	return New(&config.Config{
		Weights: config.Weights{Latency: 0.6, Stability: 0.3, Bandwidth: 0.1},
	})
}

// top 返回候选列表中评分最高的线路名，无候选时返回空串。
func top(s *Selector) string {
	c := s.Candidates()
	if len(c) == 0 {
		return ""
	}
	return c[0].Name
}

// 低延迟、高成功率的线路应当排在候选首位。
func TestBestPrefersLowerLatency(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "fast", Address: "a"}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "slow", Address: "b"}, Reachable: true,
			AvgLatency: 200 * time.Millisecond, Jitter: 30 * time.Millisecond, SuccessRate: 1},
	})

	if got := top(s); got != "fast" {
		t.Fatalf("期望首选 fast，实际 %q", got)
	}
}

// 不可达线路应被排除在候选之外。
func TestBestSkipsUnreachable(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "down", Address: "a"}, Reachable: false},
		{Line: config.Line{Name: "up", Address: "b"}, Reachable: true,
			AvgLatency: 50 * time.Millisecond, SuccessRate: 1},
	})

	c := s.Candidates()
	if len(c) != 1 || c[0].Name != "up" {
		t.Fatalf("期望候选仅含 up，实际 %+v", c)
	}
}

// 全部不可达时候选列表应为空。
func TestBestNoneReachable(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "a"}, Reachable: false},
		{Line: config.Line{Name: "b"}, Reachable: false},
	})
	if c := s.Candidates(); len(c) != 0 {
		t.Fatalf("期望无候选，实际 %+v", c)
	}
}

// 稳定性（成功率）应影响评分：延迟相近时高成功率胜出。
func TestStabilityAffectsScore(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "flaky", Address: "a"}, Reachable: true,
			AvgLatency: 50 * time.Millisecond, Jitter: 5 * time.Millisecond, SuccessRate: 0.4},
		{Line: config.Line{Name: "steady", Address: "b"}, Reachable: true,
			AvgLatency: 55 * time.Millisecond, Jitter: 5 * time.Millisecond, SuccessRate: 1},
	})
	if got := top(s); got != "steady" {
		t.Fatalf("期望稳定线路 steady 胜出，实际 %q", got)
	}
}

// 候选列表应按评分从高到低排序，供代理按序故障转移。
func TestCandidatesOrdered(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "mid", Address: "b"}, Reachable: true,
			AvgLatency: 100 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "best", Address: "a"}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "down", Address: "c"}, Reachable: false},
		{Line: config.Line{Name: "worst", Address: "d"}, Reachable: true,
			AvgLatency: 250 * time.Millisecond, SuccessRate: 1},
	})

	c := s.Candidates()
	got := make([]string, len(c))
	for i, l := range c {
		got[i] = l.Name
	}
	want := []string{"best", "mid", "worst"}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
}
