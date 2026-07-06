package selector

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/prober"
)

// 评分参考基准：用于将原始指标绝对归一化到 [0,1]。
// 采用绝对基准（而非线路间相对值）可避免把极小差异放大到满分区间。
const (
	refLatency = 300 * time.Millisecond // 延迟达到此值记 0 分，0 延迟记满分
	refJitter  = 100 * time.Millisecond // 抖动达到此值记 0 分
)

// Scored 是一条线路及其综合评分（0~100，越高越优）。
type Scored struct {
	Metrics prober.Metrics
	Score   float64
}

// Selector 依据探测指标进行综合评分，并维护当前最优线路。
type Selector struct {
	weights config.Weights

	mu      sync.RWMutex
	ranking []Scored
}

// New 创建选择器。
func New(cfg *config.Config) *Selector {
	return &Selector{weights: cfg.Weights}
}

// Update 传入一轮全部线路的探测结果，重新计算评分与排名。
func (s *Selector) Update(metrics []prober.Metrics) {
	ranking := score(metrics, s.weights)
	sort.SliceStable(ranking, func(i, j int) bool {
		return ranking[i].Score > ranking[j].Score
	})

	s.mu.Lock()
	s.ranking = ranking
	s.mu.Unlock()
}

// Candidates 返回当前所有可达线路，按评分从高到低排序。
// 供代理按序故障转移：最优线路连不上时可退到次优，避免拒绝玩家。
func (s *Selector) Candidates() []config.Line {
	s.mu.RLock()
	defer s.mu.RUnlock()

	lines := make([]config.Line, 0, len(s.ranking))
	for _, sc := range s.ranking {
		if sc.Metrics.Reachable {
			lines = append(lines, sc.Metrics.Line)
		}
	}
	return lines
}

// CandidatesForRegion 返回按玩家区域优化排序的可达线路列表。
//
// 在 Candidates（按评分降序）的基础上做稳定分组：Regions 命中玩家区域的线路整体
// 前移，其余线路（含未标记 Regions 的通用线路）按「地理就近 + 评分」综合权衡排序接在其后。
// 组内命中线路仍按评分排序，因此「同区且质量好」的线路最优先，同时保留跨区故障转移。
// 当玩家所在区域没有任何同区线路时，其余线路按「距离越近 + 评分越高越优」的综合分排序，
// 避免被分到又近又慢的线路。region 为空（无法定位）时退化为普通 Candidates。
func (s *Selector) CandidatesForRegion(region string) []config.Line {
	scored := s.candidatesScored()
	if region == "" {
		return linesOf(scored)
	}

	preferred := make([]Scored, 0, len(scored))
	rest := make([]Scored, 0, len(scored))
	for _, sc := range scored {
		if regionMatch(sc.Metrics.Line.Regions, region) {
			preferred = append(preferred, sc)
		} else {
			rest = append(rest, sc)
		}
	}

	// 无同区线路时，对其余线路按「地理就近 + 评分」综合分排序（越大越优）。
	if len(preferred) == 0 {
		sortByProximity(rest, region)
	}
	return linesOf(append(preferred, rest...))
}

// candidatesScored 返回当前所有可达线路及其评分，按评分从高到低排序。
func (s *Selector) candidatesScored() []Scored {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Scored, 0, len(s.ranking))
	for _, sc := range s.ranking {
		if sc.Metrics.Reachable {
			out = append(out, sc)
		}
	}
	return out
}

// linesOf 抽取评分列表中的线路，保持顺序。
func linesOf(scored []Scored) []config.Line {
	lines := make([]config.Line, len(scored))
	for i, sc := range scored {
		lines[i] = sc.Metrics.Line
	}
	return lines
}

// playerGeoWeight 是「就近」在玩家综合分中的权重，其余归健康度。取 0.8：
// Transfer 直连下玩家延迟由「玩家↔线路」的地理距离主导，健康度只作稳定性调节。
const playerGeoWeight = 0.8

// healthScore 返回线路健康度 [0,100]：只含成功率(0.7) + 抖动(0.3)，
// 刻意不含 prober 延迟。prober 测的是「NETTOFRP→线路」延迟，而 Transfer 直连下
// 玩家走的是「玩家→线路」，两者无关；用 prober 延迟给玩家选路会把所有玩家
// 拉向「离 NETTOFRP 近」的线路，与玩家实际就近无关。
func healthScore(m prober.Metrics) float64 {
	jitScore := invRef(float64(m.Jitter), float64(refJitter))
	stab := 0.7*m.SuccessRate + 0.3*jitScore
	return stab * 100
}

// lineDistance 返回线路到给定坐标的最近距离（公里）。线路可标多个 Regions，
// 取其中最近的一个。无任何可定位 Regions 时返回 math.MaxFloat64（视为最远）。
func lineDistance(line config.Line, plat, plon float64) float64 {
	best := math.MaxFloat64
	for _, r := range line.Regions {
		if lat, lon, ok := regionCoord(r); ok {
			if d := haversine(plat, plon, lat, lon); d < best {
				best = d
			}
		}
	}
	return best
}

// CandidatesForPlayer 按玩家真实坐标就近 + 线路健康度排序候选。
// 综合分 = playerGeoWeight*(1 - 归一化距离) + (1-playerGeoWeight)*(健康度/100)。
//
// 这是启用地理选路且成功取到玩家经纬度时的首选路径：直接以玩家实际位置就近选线，
// 完全不受 prober「NETTOFRP→线路」延迟干扰，从根本上避免所有玩家被拉向同一条
// 「离中转机近」的线路。健康度仅作次要调节，使又近又不稳的线路不至于硬胜出。
// 无可定位 Regions 的线路距离项记 0（视为最远），仅凭健康度参与排序。
func (s *Selector) CandidatesForPlayer(plat, plon float64) []config.Line {
	scored := s.candidatesScored()
	sortScoredByKey(scored, func(sc Scored) float64 {
		var proximity float64
		if d := lineDistance(sc.Metrics.Line, plat, plon); d != math.MaxFloat64 {
			proximity = clamp01(1 - d/refMaxDist)
		}
		return playerGeoWeight*proximity + (1-playerGeoWeight)*(healthScore(sc.Metrics)/100)
	})
	return linesOf(scored)
}

// geoWeight 是「地理就近」在兜底综合分中的权重，其余归评分。取 0.5 平衡二者，
// 使又近又慢的线路不会仅凭距离胜出。
const geoWeight = 0.5

// refMaxDist 是距离归一化的参考上限（公里）：约为中国东西跨度，用于把大圆距离
// 线性映射到 [0,1]。仅用于相对比较，无需精确。
const refMaxDist = 5000.0

// sortByProximity 对无同区线路的候选按「地理就近 + 评分」综合分降序稳定排序。
// 综合分 = geoWeight*(1 - 归一化距离) + (1-geoWeight)*(评分/100)。
// 无法定位坐标的线路（未标 Regions 或坐标未知）距离项记 0，仅凭评分参与排序。
func sortByProximity(scored []Scored, playerRegion string) {
	plat, plon, pok := regionCoord(playerRegion)
	if !pok {
		return // 玩家坐标未知，维持评分顺序
	}
	sortScoredByKey(scored, func(sc Scored) float64 {
		var proximity float64 // 无坐标时距离项为 0（视为最远）
		if d := lineDistance(sc.Metrics.Line, plat, plon); d != math.MaxFloat64 {
			proximity = clamp01(1 - d/refMaxDist)
		}
		return geoWeight*proximity + (1-geoWeight)*(sc.Score/100)
	})
}

// sortScoredByKey 按 key 降序稳定排序，每条线路的 key 只计算一次。
// key 常含 haversine 等较重的距离计算，若直接传给排序比较器会被反复调用；
// 预先算好各线路的 key 再排序，将距离计算从 O(n log n) 次降到 O(n) 次。
// 排序对象连同其 key 一起移动，避免比较器索引与已重排的切片脱节。
func sortScoredByKey(scored []Scored, key func(Scored) float64) {
	type keyed struct {
		sc  Scored
		key float64
	}
	items := make([]keyed, len(scored))
	for i, sc := range scored {
		items[i] = keyed{sc: sc, key: key(sc)}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].key > items[j].key
	})
	for i := range items {
		scored[i] = items[i].sc
	}
}

// regionMatch 判断线路的区域标记是否命中玩家区域。
// 玩家区域形如 "CN-ZJ"；线路标记 "CN-ZJ" 精确命中，标记 "CN" 命中同国家的玩家。
func regionMatch(lineRegions []string, playerRegion string) bool {
	country := playerRegion
	if i := strings.IndexByte(playerRegion, '-'); i > 0 {
		country = playerRegion[:i]
	}
	for _, r := range lineRegions {
		if r == playerRegion || r == country {
			return true
		}
	}
	return false
}

// Ranking 返回当前排名快照，供日志或状态查询使用。
func (s *Selector) Ranking() []Scored {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Scored, len(s.ranking))
	copy(out, s.ranking)
	return out
}

// score 对一组指标绝对归一化后加权打分。
func score(metrics []prober.Metrics, w config.Weights) []Scored {
	result := make([]Scored, 0, len(metrics))
	bwMax := maxBandwidth(metrics)

	for _, m := range metrics {
		if !m.Reachable {
			result = append(result, Scored{Metrics: m, Score: 0})
			continue
		}

		// 延迟：以 refLatency 为基准线性反向归一化，越低越好。
		latScore := invRef(float64(m.AvgLatency), float64(refLatency))

		// 稳定性：成功率为主(0.7)，抖动为辅(0.3)。
		jitScore := invRef(float64(m.Jitter), float64(refJitter))
		stabScore := 0.7*m.SuccessRate + 0.3*jitScore

		// 带宽：相对本轮最大值归一化；无法测得(0)时给中性分 0.5。
		var bwScore float64
		switch {
		case m.Bandwidth <= 0:
			bwScore = 0.5
		case bwMax <= 0:
			bwScore = 0.5
		default:
			bwScore = clamp01(m.Bandwidth / bwMax)
		}

		total := w.Latency*latScore + w.Stability*stabScore + w.Bandwidth*bwScore
		result = append(result, Scored{Metrics: m, Score: total * 100})
	}
	return result
}

// invRef 将 v 相对参考值 ref 线性反向映射到 [0,1]：v=0 得 1，v>=ref 得 0。
func invRef(v, ref float64) float64 {
	if ref <= 0 {
		return 0
	}
	return clamp01(1 - v/ref)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func maxBandwidth(ms []prober.Metrics) float64 {
	var max float64
	for _, m := range ms {
		if m.Reachable && m.Bandwidth > max {
			max = m.Bandwidth
		}
	}
	return max
}
