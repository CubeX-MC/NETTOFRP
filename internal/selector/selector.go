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
// 前移，其余线路（含未标记 Regions 的通用线路）按「地理就近 + 评分」排序接在其后。
// 组内命中线路仍按评分排序，因此「同区且质量好」的线路最优先，同时保留跨区故障转移。
// 当玩家所在区域没有任何同区线路时，其余线路按到玩家的地理距离升序排列（距离相近时
// 按评分降序），实现「就近 + 延迟」兜底。region 为空（无法定位）时退化为普通 Candidates。
func (s *Selector) CandidatesForRegion(region string) []config.Line {
	all := s.Candidates()
	if region == "" {
		return all
	}

	preferred := make([]config.Line, 0, len(all))
	rest := make([]config.Line, 0, len(all))
	for _, line := range all {
		if regionMatch(line.Regions, region) {
			preferred = append(preferred, line)
		} else {
			rest = append(rest, line)
		}
	}

	// 无同区线路时，对其余线路按到玩家的地理距离就近排序（原顺序即评分降序，
	// 作为距离相近或无坐标时的次级依据，sort.SliceStable 保持其稳定性）。
	if len(preferred) == 0 {
		sortByProximity(rest, region)
	}
	return append(preferred, rest...)
}

// sortByProximity 依据线路首个可定位区域到玩家区域的地理距离升序稳定排序。
// 无法定位坐标的线路（未标 Regions 或坐标未知）视为最远，排在末尾但保持原评分顺序。
func sortByProximity(lines []config.Line, playerRegion string) {
	plat, plon, pok := regionCoord(playerRegion)
	if !pok {
		return // 玩家坐标未知，维持评分顺序
	}
	dist := func(l config.Line) float64 {
		best := math.MaxFloat64
		for _, r := range l.Regions {
			if lat, lon, ok := regionCoord(r); ok {
				if d := haversine(plat, plon, lat, lon); d < best {
					best = d
				}
			}
		}
		return best
	}
	sort.SliceStable(lines, func(i, j int) bool {
		return dist(lines[i]) < dist(lines[j])
	})
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
