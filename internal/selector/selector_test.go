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

// names 抽取线路名列表，便于断言顺序。
func names(lines []config.Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Name
	}
	return out
}

// 同区线路应整体前移到候选前列，即便其评分低于跨区线路；
// 组内仍按评分排序，未标记 Regions 的通用线路排在同区线路之后。
func TestCandidatesForRegionPrefersSameRegion(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		// 跨区线路评分最高（延迟最低）。
		{Line: config.Line{Name: "gd-fast", Regions: []string{"CN-GD"}}, Reachable: true,
			AvgLatency: 10 * time.Millisecond, SuccessRate: 1},
		// 同区线路评分次之。
		{Line: config.Line{Name: "zj-mid", Regions: []string{"CN-ZJ"}}, Reachable: true,
			AvgLatency: 60 * time.Millisecond, SuccessRate: 1},
		// 通用线路（无 Regions），评分居中。
		{Line: config.Line{Name: "any", Regions: nil}, Reachable: true,
			AvgLatency: 40 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForRegion("CN-ZJ"))
	// 同区 zj-mid 前移到首位，其余按原评分顺序：gd-fast(10ms) > any(40ms)。
	want := []string{"zj-mid", "gd-fast", "any"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
}

// 线路以国家码 "CN" 标记时，应命中同国家任意省份的玩家。
func TestCandidatesForRegionCountryLevelMatch(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "overseas", Regions: []string{"US"}}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "cn-any", Regions: []string{"CN"}}, Reachable: true,
			AvgLatency: 80 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForRegion("CN-ZJ"))
	if len(got) == 0 || got[0] != "cn-any" {
		t.Fatalf("期望 CN 标记线路 cn-any 命中 CN-ZJ 玩家并前移，实际 %v", got)
	}
}

// region 为空（无法定位）时应退化为普通评分排序。
func TestCandidatesForRegionEmptyFallsBack(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "a", Regions: []string{"CN-ZJ"}}, Reachable: true,
			AvgLatency: 100 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "b", Regions: []string{"CN-GD"}}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForRegion(""))
	want := []string{"b", "a"} // 纯按评分
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
}

// 玩家所在区域无同区线路时，其余线路应按地理就近排序：河南(CN-HA)玩家
// 应优先分到地理更近的山东(CN-SD)线路，而非评分更高但更远的广东(CN-GD)线路。
func TestCandidatesForRegionProximityFallback(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		// 广东线路评分最高（延迟最低），但离河南远。
		{Line: config.Line{Name: "gd", Regions: []string{"CN-GD"}}, Reachable: true,
			AvgLatency: 10 * time.Millisecond, SuccessRate: 1},
		// 山东线路评分次之，但离河南近。
		{Line: config.Line{Name: "sd", Regions: []string{"CN-SD"}}, Reachable: true,
			AvgLatency: 40 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForRegion("CN-HA"))
	if len(got) == 0 || got[0] != "sd" {
		t.Fatalf("河南玩家无同区线路时应就近选山东 sd，实际 %v", got)
	}
}

// 综合权衡：最近的线路若延迟极高（评分很低），不应仅凭距离被硬选中。
// 河南玩家面对「近但极慢的山东(244ms)」与「稍远但很快的北京(27ms)」应选北京。
func TestCandidatesForRegionProximityRespectsScore(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		// 山东离河南最近，但延迟极高。
		{Line: config.Line{Name: "sd-slow", Regions: []string{"CN-SD"}}, Reachable: true,
			AvgLatency: 244 * time.Millisecond, SuccessRate: 1},
		// 北京稍远，但延迟很低。
		{Line: config.Line{Name: "bj-fast", Regions: []string{"CN-BJ"}}, Reachable: true,
			AvgLatency: 27 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForRegion("CN-HA"))
	if len(got) == 0 || got[0] != "bj-fast" {
		t.Fatalf("近但极慢的线路不应被硬选，期望首选 bj-fast，实际 %v", got)
	}
}

// 广州(约 23.13,113.26)玩家应就近选广东线路。即使北京线路 prober 延迟极低
// （AvgLatency 远小于广东），也不应被选中——这正是"NETTOFRP 部署在北京 →
// prober 测得北京延迟最低 → 所有玩家被拉去北京"的复现场景。CandidatesForPlayer
// 按玩家真实坐标就近选路，不使用 prober 延迟，故广东线路应稳定胜出。
func TestCandidatesForPlayerIgnoresProberLatency(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		// 北京线路：prober 延迟极低（NETTOFRP 就在北京），成功率满分。
		{Line: config.Line{Name: "bj", Regions: []string{"CN-BJ"}}, Reachable: true,
			AvgLatency: 2 * time.Millisecond, SuccessRate: 1},
		// 广东线路：prober 延迟偏高，但离广州玩家最近，健康度同样满分。
		{Line: config.Line{Name: "gd", Regions: []string{"CN-GD"}}, Reachable: true,
			AvgLatency: 60 * time.Millisecond, SuccessRate: 1},
	})

	// 广州坐标。
	got := names(s.CandidatesForPlayer(23.13, 113.26))
	if len(got) == 0 || got[0] != "gd" {
		t.Fatalf("广州玩家应就近选广东 gd（不受北京 prober 低延迟干扰），实际 %v", got)
	}
}

// 就近为主但健康度仍作调节：最近的线路若成功率极低（不稳定），
// 不应仅凭距离硬胜出。北京玩家面对「近但频繁掉线的北京(成功率0.2)」与
// 「稍远但稳定的天津(成功率1)」，综合权衡后应避开极不稳定的北京线路。
func TestCandidatesForPlayerHealthAdjusts(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		// 北京离玩家最近，但成功率极低。
		{Line: config.Line{Name: "bj-flaky", Regions: []string{"CN-BJ"}}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, Jitter: 80 * time.Millisecond, SuccessRate: 0.2},
		// 天津紧邻北京（约 39.13,117.20），距离几乎相同，但完全稳定。
		{Line: config.Line{Name: "tj-steady", Regions: []string{"CN-TJ"}}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, SuccessRate: 1},
	})

	// 北京玩家坐标。
	got := names(s.CandidatesForPlayer(39.90, 116.41))
	if len(got) == 0 || got[0] != "tj-steady" {
		t.Fatalf("距离几乎相同时，极不稳定线路不应胜出，期望 tj-steady，实际 %v", got)
	}
}
