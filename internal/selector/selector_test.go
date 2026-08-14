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

func TestScoreFallsBackToAverageWhenMinimumMissing(t *testing.T) {
	got := score([]prober.Metrics{{
		Line:        config.Line{Name: "line"},
		Reachable:   true,
		AvgLatency:  300 * time.Millisecond,
		SuccessRate: 1,
	}}, config.Weights{Latency: 1}, 2.0)

	if len(got) != 1 || got[0].Score != 0 {
		t.Fatalf("缺少最小延迟时应回退到平均延迟，实际评分 %+v", got)
	}
}

func TestEMAResetsAfterUnreachableSample(t *testing.T) {
	s := newSel()
	line := config.Line{Name: "line"}
	s.Update([]prober.Metrics{{
		Line: line, Reachable: true, AvgLatency: 20 * time.Millisecond,
		MinLatency: 10 * time.Millisecond, SuccessRate: 1,
	}})
	s.Update([]prober.Metrics{{Line: line, Reachable: false}})
	s.Update([]prober.Metrics{{
		Line: line, Reachable: true, AvgLatency: 200 * time.Millisecond,
		MinLatency: 150 * time.Millisecond, SuccessRate: 1,
	}})

	ranking := s.Ranking()
	if len(ranking) != 1 {
		t.Fatalf("期望一条排名记录，实际 %+v", ranking)
	}
	if got := ranking[0].Metrics.AvgLatency; got != 200*time.Millisecond {
		t.Fatalf("恢复后的首个样本不应混入故障前历史，期望 200ms，实际 %v", got)
	}
	if got := ranking[0].Metrics.MinLatency; got != 150*time.Millisecond {
		t.Fatalf("恢复后的最小延迟应重新初始化，期望 150ms，实际 %v", got)
	}
}

func TestEMASmoothsReachableSamples(t *testing.T) {
	s := newSel()
	line := config.Line{Name: "line"}
	s.Update([]prober.Metrics{{
		Line: line, Reachable: true, AvgLatency: 100 * time.Millisecond,
		MinLatency: 50 * time.Millisecond, SuccessRate: 1,
	}})
	s.Update([]prober.Metrics{{
		Line: line, Reachable: true, AvgLatency: 200 * time.Millisecond,
		MinLatency: 150 * time.Millisecond, SuccessRate: 0.5,
	}})

	got := s.Ranking()[0].Metrics
	if got.AvgLatency != 130*time.Millisecond || got.MinLatency != 80*time.Millisecond {
		t.Fatalf("EMA 延迟结果错误：avg=%v min=%v", got.AvgLatency, got.MinLatency)
	}
	if got.SuccessRate < 0.849999 || got.SuccessRate > 0.850001 {
		t.Fatalf("EMA 成功率结果错误：%v", got.SuccessRate)
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

func TestCandidatesForPlayerSupportsCountryLevelOverseasLines(t *testing.T) {
	tests := []struct {
		name       string
		playerLat  float64
		playerLon  float64
		nearRegion string
		farRegion  string
	}{
		{name: "HongKong", playerLat: 22.32, playerLon: 114.17, nearRegion: "HK", farRegion: "CN-SH"},
		{name: "Tokyo", playerLat: 35.68, playerLon: 139.76, nearRegion: "JP", farRegion: "CN-SH"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSel()
			s.Update([]prober.Metrics{
				{Line: config.Line{Name: "near", Regions: []string{tc.nearRegion}}, Reachable: true,
					AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
				{Line: config.Line{Name: "far", Regions: []string{tc.farRegion}}, Reachable: true,
					AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
			})

			got := names(s.CandidatesForPlayer(tc.playerLat, tc.playerLon))
			if len(got) == 0 || got[0] != "near" {
				t.Fatalf("玩家应优先命中国家级线路 %s，实际 %v", tc.nearRegion, got)
			}
		})
	}
}

// 非线性延迟评分：曲线 1-x^exp 在 x∈(0,1) 上始终 ≥ 线性（1-x），
// 但低延迟段的差异被压缩（两个低延迟线路之间评分差变小），
// 高延迟段的差异被放大（两个高延迟线路之间评分差变大）。
func TestLatencyScoreNonlinearBehavior(t *testing.T) {
	// 低延迟段（50ms vs 100ms）差异应被压缩（非线性差异 < 线性差异）。
	diffExp2Low := latencyScore(50e6, 300e6, 2.0) - latencyScore(100e6, 300e6, 2.0)
	diffLinearLow := latencyScore(50e6, 300e6, 1.0) - latencyScore(100e6, 300e6, 1.0)
	if diffExp2Low >= diffLinearLow {
		t.Fatalf("低延迟段差异应被压缩（非线性 %.4f < 线性 %.4f）", diffExp2Low, diffLinearLow)
	}
	// 高延迟段（200ms vs 250ms）差异应被放大（非线性差异 > 线性差异）。
	diffExp2High := latencyScore(200e6, 300e6, 2.0) - latencyScore(250e6, 300e6, 2.0)
	diffLinearHigh := latencyScore(200e6, 300e6, 1.0) - latencyScore(250e6, 300e6, 1.0)
	if diffExp2High <= diffLinearHigh {
		t.Fatalf("高延迟段差异应被放大（非线性 %.4f > 线性 %.4f）", diffExp2High, diffLinearHigh)
	}
	// 达到基准值（300ms）时两者均为 0。
	atRef := latencyScore(300e6, 300e6, 2.0)
	if atRef != 0 {
		t.Fatalf("300ms 时评分为 0，实际 %.4f", atRef)
	}
}

// 恢复期惩罚：连续失败后恢复的线路评分应低于同质量但从未失败的线路。
func TestRecoveryPenaltyReducesScore(t *testing.T) {
	s := newSel()
	lineA := config.Line{Name: "steady", Address: "a"}
	lineB := config.Line{Name: "recovered", Address: "b"}

	// 两轮：线路 A 一直正常，线路 B 第一次失败、第二次恢复。
	s.Update([]prober.Metrics{
		{Line: lineA, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: lineB, Reachable: false},
	})
	s.Update([]prober.Metrics{
		{Line: lineA, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: lineB, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
	})

	ranking := s.Ranking()
	if len(ranking) != 2 {
		t.Fatalf("期望两条排名记录，实际 %d", len(ranking))
	}

	// 线路 B 刚恢复，应受到惩罚，评分低于完全相同的线路 A。
	if ranking[0].Metrics.Line.Name != "steady" {
		t.Fatalf("稳定线路应排在首位，实际 %s", ranking[0].Metrics.Line.Name)
	}
	if ranking[1].Score >= ranking[0].Score {
		t.Fatalf("刚恢复的线路评分应低于稳定线路（%.2f vs %.2f）", ranking[1].Score, ranking[0].Score)
	}
}

// 恢复期惩罚应逐步衰减：连续成功轮次后，罚分系数应恢复至 1。
func TestRecoveryPenaltyDecays(t *testing.T) {
	line := config.Line{Name: "flap", Address: "a"}
	s := newSel()

	// 一轮失败。
	s.Update([]prober.Metrics{{Line: line, Reachable: false}})
	// 恢复后连续成功若干轮，惩罚逐步衰减。
	for i := 0; i < 6; i++ {
		s.Update([]prober.Metrics{
			{Line: line, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		})
	}

	// 6 轮后的惩罚系数应为 1（无惩罚）。
	if p := s.Penalty(line.Name); p != 1 {
		t.Fatalf("6 轮恢复后惩罚系数应为 1，实际 %.2f", p)
	}
}

// 健康度叠加惩罚：地理选路中，刚恢复的线路健康度应降低。
func TestCandidatesForPlayerRespectsRecoveryPenalty(t *testing.T) {
	s := newSel()
	lineA := config.Line{Name: "bj-recovered", Regions: []string{"CN-BJ"}}
	lineB := config.Line{Name: "tj-steady", Regions: []string{"CN-TJ"}}

	// 初始：北京连续 3 轮失败，天津一直正常。
	for i := 0; i < 3; i++ {
		s.Update([]prober.Metrics{
			{Line: lineA, Reachable: false},
			{Line: lineB, Reachable: true, AvgLatency: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, SuccessRate: 1},
		})
	}
	// 恢复后：北京恢复，天津正常。北京离玩家更近，但因受恢复期惩罚仍应排在天津之后。
	s.Update([]prober.Metrics{
		{Line: lineA, Reachable: true, AvgLatency: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, SuccessRate: 1},
		{Line: lineB, Reachable: true, AvgLatency: 20 * time.Millisecond, Jitter: 2 * time.Millisecond, SuccessRate: 1},
	})

	got := names(s.CandidatesForPlayer(39.90, 116.41)) // 北京玩家
	if len(got) == 0 || got[0] != "tj-steady" {
		t.Fatalf("刚恢复的北京线路不应因距离近而被选中，期望 tj-steady，实际 %v", got)
	}
}

// 滞回：评分接近的两条线路不应在每轮探测后反复翻转。
func TestApplyStickyPreventsFlipping(t *testing.T) {
	// 线路 a 评分 95，b 评分 96（差 1 < 阈值 2），应保持 a 在首位。
	ranking := []Scored{
		{Metrics: prober.Metrics{Line: config.Line{Name: "b"}, Reachable: true}, Score: 96},
		{Metrics: prober.Metrics{Line: config.Line{Name: "a"}, Reachable: true}, Score: 95},
	}

	got, sticky := applySticky(ranking, "a")
	if sticky != "a" {
		t.Fatalf("滞回应保持粘性线路 a，实际 %q", sticky)
	}
	if got[0].Metrics.Line.Name != "a" {
		t.Fatalf("粘性线路 a 应前移到首位，实际 %q", got[0].Metrics.Line.Name)
	}
}

// 滞回：新候选评分明显超越粘性线路时应切换。
func TestApplyStickySwitchesWhenClearlyBetter(t *testing.T) {
	// 线路 b 评分 98，a 评分 95（差 3 > 阈值 2），应切换到 b。
	ranking := []Scored{
		{Metrics: prober.Metrics{Line: config.Line{Name: "b"}, Reachable: true}, Score: 98},
		{Metrics: prober.Metrics{Line: config.Line{Name: "a"}, Reachable: true}, Score: 95},
	}

	got, sticky := applySticky(ranking, "a")
	if sticky != "b" {
		t.Fatalf("明显更优的线路 b 应成为新粘性首选，实际 %q", sticky)
	}
	if got[0].Metrics.Line.Name != "b" {
		t.Fatalf("线路 b 应保持在首位，实际 %q", got[0].Metrics.Line.Name)
	}
}

// 滞回：粘性线路掉线时应立即解除锁定，切换到次优。
func TestApplyStickyReleasesWhenDown(t *testing.T) {
	ranking := []Scored{
		{Metrics: prober.Metrics{Line: config.Line{Name: "b"}, Reachable: true}, Score: 90},
		{Metrics: prober.Metrics{Line: config.Line{Name: "a"}, Reachable: false}, Score: 0},
	}

	got, sticky := applySticky(ranking, "a")
	if sticky != "b" {
		t.Fatalf("粘性线路掉线后应切换到 b，实际 %q", sticky)
	}
	if got[0].Metrics.Line.Name != "b" {
		t.Fatalf("线路 b 应为首位，实际 %q", got[0].Metrics.Line.Name)
	}
}

// 首次运行（无粘性线路）应直接锁定评分最高者。
func TestApplyStickyInitial(t *testing.T) {
	ranking := []Scored{
		{Metrics: prober.Metrics{Line: config.Line{Name: "a"}, Reachable: true}, Score: 95},
		{Metrics: prober.Metrics{Line: config.Line{Name: "b"}, Reachable: true}, Score: 90},
	}

	got, sticky := applySticky(ranking, "")
	if sticky != "a" {
		t.Fatalf("首次运行应锁定评分最高者 a，实际 %q", sticky)
	}
	if got[0].Metrics.Line.Name != "a" {
		t.Fatalf("线路 a 应为首位，实际 %q", got[0].Metrics.Line.Name)
	}
}

// 中位数延迟应参与评分：median 抗尖峰，令含高延迟尖峰样本的线路得分
// 高于仅用均值时的结果。
func TestScoreUsesMedianLatency(t *testing.T) {
	// 一条线路均值被尖峰拉高（avg=200ms），但中位数稳定（median=50ms, min=40ms）。
	got := score([]prober.Metrics{{
		Line:          config.Line{Name: "line"},
		Reachable:     true,
		MinLatency:    40 * time.Millisecond,
		MedianLatency: 50 * time.Millisecond,
		AvgLatency:    200 * time.Millisecond,
		SuccessRate:   1,
	}}, config.Weights{Latency: 1}, 2.0)

	if len(got) != 1 {
		t.Fatalf("期望一条评分记录，实际 %d", len(got))
	}
	// mixed = 0.4*40 + 0.6*50 = 46ms，应远高于仅用 avg(200ms) 的得分。
	// 46ms → 1-(46/300)^2 ≈ 0.9765，评分约 97.65。
	if got[0].Score < 90 {
		t.Fatalf("中位数应显著降低尖峰影响，实际评分 %.2f", got[0].Score)
	}
}

// 自适应探测间隔：全部可达且稳定足够轮数时应拉长间隔。
func TestRecommendedIntervalSlowsWhenStable(t *testing.T) {
	s := newSel()
	line := config.Line{Name: "a", Address: "x"}
	// 连续 5 轮全部可达且首选不变。
	for i := 0; i < 5; i++ {
		s.Update([]prober.Metrics{
			{Line: line, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		})
	}
	base := 15 * time.Second
	if got := s.RecommendedInterval(base); got != base*2 {
		t.Fatalf("稳定 5 轮后间隔应拉长到 %v，实际 %v", base*2, got)
	}
}

// 自适应探测间隔：存在不可达线路时应缩短间隔以便尽快感知恢复。
func TestRecommendedIntervalSpeedsWhenUnreachable(t *testing.T) {
	s := newSel()
	s.Update([]prober.Metrics{
		{Line: config.Line{Name: "up", Address: "a"}, Reachable: true,
			AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: config.Line{Name: "down", Address: "b"}, Reachable: false},
	})
	base := 15 * time.Second
	if got := s.RecommendedInterval(base); got != base/3 {
		t.Fatalf("有不可达线路时间隔应缩短到 %v，实际 %v", base/3, got)
	}
}

// 连接数感知：窗口内被频繁选择的线路应获得负载惩罚，从而让位给空闲线路。
func TestLoadBalancingPenalizesBusyLine(t *testing.T) {
	s := newSel()
	lineA := config.Line{Name: "a", Address: "x"}
	lineB := config.Line{Name: "b", Address: "y"}

	// 两轮都可达且质量完全相同（避免质量差导致排序差异）。
	metrics := []prober.Metrics{
		{Line: lineA, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: lineB, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
	}
	s.Update(metrics)

	// 大量选择线路 a（模拟玩家都涌入 a），少量选择线路 b。
	for i := 0; i < 10; i++ {
		s.RecordSelection("a")
	}
	s.RecordSelection("b")

	// 再次更新评分：a 因负载惩罚应排在 b 之后。
	s.Update(metrics)
	got := names(s.Candidates())
	if len(got) == 0 || got[0] != "b" {
		t.Fatalf("高负载线路 a 应让位给空闲线路 b，实际 %v", got)
	}
}

// 连接数感知：负载记录在窗口过期后失效，线路恢复正常排序。
func TestLoadBalancingExpires(t *testing.T) {
	s := newSel()
	lineA := config.Line{Name: "a", Address: "x"}
	lineB := config.Line{Name: "b", Address: "y"}

	metrics := []prober.Metrics{
		{Line: lineA, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
		{Line: lineB, Reachable: true, AvgLatency: 20 * time.Millisecond, SuccessRate: 1},
	}
	s.Update(metrics)
	for i := 0; i < 10; i++ {
		s.RecordSelection("a")
	}

	// 窗口过期（loadWindow 后）后 a 不再受惩罚。
	s.selections = nil // 直接清空，模拟窗口过期清理
	s.Update(metrics)
	got := names(s.Candidates())
	if len(got) == 0 || got[0] != "a" {
		t.Fatalf("负载记录过期后应恢复原排序，期望 a，实际 %v", got)
	}
}
