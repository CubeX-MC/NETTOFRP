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
	refLatency            = 300 * time.Millisecond // 延迟达到此值记 0 分，0 延迟记满分
	refJitter             = 100 * time.Millisecond // 抖动达到此值记 0 分
	defaultLatencyExponent = 2.0                   // 默认延迟评分非线性指数
	maxConsecutiveFails   = 5                      // 连续失败计数上限（超过后惩罚封底）
)

// Scored 是一条线路及其综合评分（0~100，越高越优）。
type Scored struct {
	Metrics prober.Metrics
	Score   float64
}

// emaAlpha 是 EMA 平滑系数。越小越平滑（历史权重越大），越大越跟随最新值。
// 0.3 表示新数据占 30%，历史占 70%，约需 5~6 轮才能充分响应趋势变化。
const emaAlpha = 0.3

// healthThreshold 是健康度底线 [0,100]。低于此值的线路被视为"差线路"，
// 在地理就近排序中降至末尾，不与健康线路竞争距离优势。
const healthThreshold = 40.0

// switchThreshold 是首选线路切换的滞回阈值（评分分差，0~100 制）。
// 只有当新候选的评分比当前粘性首选高出该分差时才会切换首选，
// 避免两条评分接近的线路在每轮探测后反复翻转，令玩家被 Transfer 到不同线路。
const switchThreshold = 2.0

// 自适应探测间隔阈值：stableRounds 达到该值后认为线路处于稳定期，
// 建议拉长探测间隔以降低无谓探测；stickyRounds 归零（切换/故障）时建议缩短。
const (
	stableRoundsForSlow  = 4 // 首选连续稳定轮数达到该值 → 拉长间隔
	adaptiveIntervalFast = 3 // 快速探测间隔 = 基准/3
	adaptiveIntervalSlow = 2 // 慢速探测间隔 = 基准×2
)

// loadWindow 是连接数感知选路的统计窗口：统计该时间段内各线路被 Transfer
// 下发的次数。玩家直连线路后流量不再经过代理，代理无法感知真实在线数，
// 用"近期进入的玩家数"近似负载。
const loadWindow = 60 * time.Second

// maxLoadPenalty 是负载惩罚的最大幅度：最忙线路评分最多被压低的比例。
// 取 0.15 时，最忙线路降 15%，既足以分流，又不至于让负载完全压过质量差异。
const maxLoadPenalty = 0.15

// Selector 依据探测指标进行综合评分，并维护当前最优线路。
type Selector struct {
	weights    config.Weights
	latencyExp float64

	mu     sync.RWMutex
	ranking []Scored
	ema     map[string]emaState // 线路名 → EMA 历史状态
	sticky  string              // 粘性首选线路名（滞回抑制频繁翻转）

	// stableRounds 统计粘性首选连续未变的轮数，用于自适应探测间隔。
	// 切换首选、出现故障或恢复期惩罚时归零。
	stableRounds int

	// 连接数感知选路：loadMu 保护 selections。
	// selections 记录每条线路在 loadWindow 窗口内被 Transfer 下发的次数，
	// 用于对高负载线路施加评分惩罚，避免所有玩家涌入同一条线路。
	loadMu      sync.Mutex
	selections  map[string][]time.Time
}

// emaState 保存各指标的 EMA 历史值及连续失败计数。
type emaState struct {
	avgLatency    float64 // ns
	minLatency    float64 // ns
	medianLatency float64 // ns
	jitter        float64 // ns
	successRate   float64
	bandwidth     float64
	initialized   bool
	fails         int // 连续不可达轮次（用于恢复期评分惩罚）
}

// New 创建选择器。
func New(cfg *config.Config) *Selector {
	exp := cfg.LatencyScoreExponent
	if exp <= 0 {
		exp = defaultLatencyExponent
	}
	return &Selector{
		weights:    cfg.Weights,
		latencyExp: exp,
		ema:        make(map[string]emaState),
		selections: make(map[string][]time.Time),
	}
}

// Update 传入一轮全部线路的探测结果，先做 EMA 平滑再重新计算评分与排名。
func (s *Selector) Update(metrics []prober.Metrics) {
	s.mu.Lock()
	defer s.mu.Unlock()

	smoothed := make([]prober.Metrics, len(metrics))
	for i, m := range metrics {
		smoothed[i] = s.applyEMA(m)
	}

	ranking := score(smoothed, s.weights, s.latencyExp)
	for i := range ranking {
		if st, ok := s.ema[ranking[i].Metrics.Line.Name]; ok {
			ranking[i].Score *= failurePenalty(st.fails)
		}
	}
	// 连接数感知：对窗口内被频繁 Transfer 的线路施加负载惩罚，促进分流。
	loadFactors := s.computeLoadFactors()
	for i := range ranking {
		if f, ok := loadFactors[ranking[i].Metrics.Line.Name]; ok {
			ranking[i].Score *= f
		}
	}
	sort.SliceStable(ranking, func(i, j int) bool {
		return ranking[i].Score > ranking[j].Score
	})
	// 滞回：抑制首选线路在评分接近时反复翻转。
	oldSticky := s.sticky
	ranking, s.sticky = applySticky(ranking, s.sticky)
	s.ranking = ranking
	s.updateStableRounds(oldSticky)
}

// updateStableRounds 更新稳定轮次计数：粘性首选发生真实切换（非首次锁定）、
// 出现不可达线路或存在恢复期惩罚线路时归零，否则累加。
// 必须在持有 s.mu 写锁时调用。
func (s *Selector) updateStableRounds(oldSticky string) {
	unstable := oldSticky != "" && s.sticky != oldSticky
	if !unstable {
		for _, sc := range s.ranking {
			if !sc.Metrics.Reachable {
				unstable = true
				break
			}
		}
	}
	if !unstable {
		for _, st := range s.ema {
			if st.fails > 0 {
				unstable = true
				break
			}
		}
	}
	if unstable {
		s.stableRounds = 0
	} else {
		s.stableRounds++
	}
}

// RecommendedInterval 返回下一轮建议的探测间隔：
//   - 有不可达线路、恢复期惩罚或首选刚切换 → base/3（快速探测，尽快感知恢复与新最优）
//   - 首选连续稳定 ≥ stableRoundsForSlow 轮 → base×2（降低无谓探测）
//   - 其余 → base
func (s *Selector) RecommendedInterval(base time.Duration) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, sc := range s.ranking {
		if !sc.Metrics.Reachable {
			return base / adaptiveIntervalFast
		}
	}
	for _, st := range s.ema {
		if st.fails > 0 {
			return base / adaptiveIntervalFast
		}
	}
	if s.stableRounds == 0 {
		return base / adaptiveIntervalFast
	}
	if s.stableRounds >= stableRoundsForSlow {
		return base * adaptiveIntervalSlow
	}
	return base
}

// applySticky 对已按真实评分降序的排名应用滞回，返回调整后的排名与新粘性首选。
//
// 规则：
//   - 无候选或粘性线路不可达/消失 → 粘性 = 当前首位（自动解除锁定）
//   - 粘性线路仍是首位 → 保持不动
//   - 新首位评分 - 粘性线路评分 > switchThreshold → 切换粘性到新首位
//   - 否则（差距在阈值内）→ 粘性线路前移到首位，抑制翻转
//
// 该函数为纯函数，便于单测。
func applySticky(ranking []Scored, sticky string) ([]Scored, string) {
	if len(ranking) == 0 {
		return ranking, ""
	}

	best := ranking[0]
	stickyScore, stickyReachable := scoreOfReachable(ranking, sticky)

	// 首次、粘性线路不存在或不可达：直接锁定当前首位。
	if sticky == "" || !stickyReachable {
		return ranking, best.Metrics.Line.Name
	}
	// 粘性线路已是首位：保持。
	if best.Metrics.Line.Name == sticky {
		return ranking, sticky
	}
	// 新首位明显超越才切换，否则粘性线路前移。
	if best.Score-stickyScore > switchThreshold {
		return ranking, best.Metrics.Line.Name
	}
	return moveToFront(ranking, sticky), sticky
}

// scoreOfReachable 返回指定线路在排名中的评分及是否可达。
func scoreOfReachable(ranking []Scored, name string) (float64, bool) {
	for _, sc := range ranking {
		if sc.Metrics.Line.Name == name {
			return sc.Score, sc.Metrics.Reachable
		}
	}
	return 0, false
}

// moveToFront 把指定线路移到切片首位，保持其余线路相对顺序不变。
// 未找到时原样返回。
func moveToFront(scored []Scored, name string) []Scored {
	for i, sc := range scored {
		if sc.Metrics.Line.Name == name {
			copy(scored[1:i+1], scored[0:i])
			scored[0] = sc
			return scored
		}
	}
	return scored
}

// applyEMA 对单条线路的指标做 EMA 平滑，返回平滑后的 Metrics。
// 不可达时保留历史但记录连续失败计数；恢复后从新观测重新初始化，并施加惩罚。
// 必须在持有 s.mu 写锁时调用。
func (s *Selector) applyEMA(m prober.Metrics) prober.Metrics {
	key := m.Line.Name
	prev, ok := s.ema[key]

	if !m.Reachable {
		// 保留历史（供恢复参考），但初始化标记置 false 使恢复后重新建立 EMA。
		// 连续失败计数累加，用于恢复期评分惩罚。
		if ok {
			prev.fails++
			if prev.fails > maxConsecutiveFails {
				prev.fails = maxConsecutiveFails
			}
			prev.initialized = false
		} else {
			prev = emaState{fails: 1}
		}
		s.ema[key] = prev
		return m
	}

	minLatency := effectiveMinLatency(m)
	fails := 0
	if ok {
		fails = prev.fails
	}
	medLatency := effectiveMedianLatency(m)
	if !ok || !prev.initialized {
		// 无历史或刚从故障恢复：从新观测初始化，保留失败计数以施加惩罚。
		s.ema[key] = emaState{
			avgLatency:    float64(m.AvgLatency),
			minLatency:    float64(minLatency),
			medianLatency: float64(medLatency),
			jitter:        float64(m.Jitter),
			successRate:   m.SuccessRate,
			bandwidth:     m.Bandwidth,
			initialized:   true,
			fails:         fails,
		}
		m.MinLatency = minLatency
		m.MedianLatency = medLatency
		return m
	}

	// EMA：new = alpha*current + (1-alpha)*prev
	next := emaState{
		avgLatency:    emaAlpha*float64(m.AvgLatency) + (1-emaAlpha)*prev.avgLatency,
		minLatency:    emaAlpha*float64(minLatency) + (1-emaAlpha)*prev.minLatency,
		medianLatency: emaAlpha*float64(medLatency) + (1-emaAlpha)*prev.medianLatency,
		jitter:        emaAlpha*float64(m.Jitter) + (1-emaAlpha)*prev.jitter,
		successRate:   emaAlpha*m.SuccessRate + (1-emaAlpha)*prev.successRate,
		bandwidth:     emaAlpha*m.Bandwidth + (1-emaAlpha)*prev.bandwidth,
		initialized:   true,
		// 恢复观察期：每个成功轮次失败计数递减，惩罚逐步释放
		fails: decayFails(prev.fails),
	}
	s.ema[key] = next

	out := m
	out.AvgLatency = time.Duration(next.avgLatency)
	out.MinLatency = time.Duration(next.minLatency)
	out.MedianLatency = time.Duration(next.medianLatency)
	out.Jitter = time.Duration(next.jitter)
	out.SuccessRate = next.successRate
	out.Bandwidth = next.bandwidth
	return out
}

// failurePenalty 返回连续失败后的恢复期评分惩罚系数 [0.5, 1]。
// 失败 1 次 → 0.9，4 次及以上 → 0.5 封底。无失败时返回 1（无惩罚）。
func failurePenalty(fails int) float64 {
	if fails <= 0 {
		return 1
	}
	if p := 1 - 0.1*float64(fails); p > 0.5 {
		return p
	}
	return 0.5
}

// decayFails 成功轮次对连续失败计数的衰减：每轮 -1，直至 0。
func decayFails(fails int) int {
	if fails <= 0 {
		return 0
	}
	return fails - 1
}

// Penalty 返回线路当前的恢复期惩罚系数（内部读锁安全）。
func (s *Selector) Penalty(lineName string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.ema[lineName]; ok {
		return failurePenalty(st.fails)
	}
	return 1
}

// RecordSelection 记录一次将玩家 Transfer 到某条线路的选择事件。
// 由代理在成功下发 Transfer 包后调用，用于连接数感知的负载统计。
func (s *Selector) RecordSelection(lineName string) {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	now := time.Now()
	s.selections[lineName] = append(s.selections[lineName], now)
	s.pruneSelections(now)
}

// pruneSelections 清理窗口外的选择记录。必须在持有 loadMu 时调用。
func (s *Selector) pruneSelections(now time.Time) {
	cutoff := now.Add(-loadWindow)
	for name, ts := range s.selections {
		n := 0
		for _, t := range ts {
			if t.After(cutoff) {
				ts[n] = t
				n++
			}
		}
		if n == 0 {
			delete(s.selections, name)
		} else {
			s.selections[name] = ts[:n]
		}
	}
}

// computeLoadFactors 计算各线路的负载惩罚系数 map[lineName]factor。
// 惩罚是相对的：窗口内选择次数最多的线路 factor = 1-maxLoadPenalty（最重），
// 最少的线路 factor = 1（无惩罚），其余线性过渡。全部相同或无记录时均返回 1，
// 避免「所有线路都有玩家」时整体降分。
func (s *Selector) computeLoadFactors() map[string]float64 {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.pruneSelections(time.Now())

	if len(s.selections) == 0 {
		return nil
	}
	min, max := int(^uint(0)>>1), 0
	for _, ts := range s.selections {
		c := len(ts)
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}
	if max == min {
		return nil // 所有线路负载相同，不施加惩罚
	}

	factors := make(map[string]float64, len(s.selections))
	span := float64(max - min)
	for name, ts := range s.selections {
		ratio := float64(len(ts)-min) / span // [0,1]，越大越忙
		factors[name] = 1 - maxLoadPenalty*ratio
	}
	return factors
}

// loadFactor 返回单条线路的负载惩罚系数（读锁安全）。无记录或负载与
// 其他线路无差异时返回 1。公式与 computeLoadFactors 一致：按相对最忙比例线性惩罚。
func (s *Selector) loadFactor(lineName string) float64 {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.pruneSelections(time.Now())

	ts, ok := s.selections[lineName]
	if !ok || len(ts) == 0 || len(s.selections) < 2 {
		return 1
	}
	min, max := int(^uint(0)>>1), 0
	for _, other := range s.selections {
		c := len(other)
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}
	if max == min {
		return 1
	}
	ratio := float64(len(ts)-min) / float64(max-min)
	return 1 - maxLoadPenalty*ratio
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

// healthScore 返回线路健康度 [0,100]：成功率(0.75) + 抖动(0.25)。
// Transfer 直连时，NETTOFRP 到线路的延迟不代表玩家到线路的延迟，
// 因此不将探测延迟混入玩家就近排序的健康度。
func healthScore(m prober.Metrics) float64 {
	jitScore := invRef(float64(m.Jitter), float64(refJitter))
	return (0.75*m.SuccessRate + 0.25*jitScore) * 100
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
// 健康度低于 healthThreshold 的线路被视为"差线路"，直接降至末尾，
// 不与健康线路竞争距离优势，避免选出又远又差的线路。
// 综合分 = playerGeoWeight*(1 - 归一化距离) + (1-playerGeoWeight)*(健康度/100)。
// 健康度已包含恢复期惩罚，确保刚恢复的线路不会以全部权重参与竞争。
func (s *Selector) CandidatesForPlayer(plat, plon float64) []config.Line {
	scored := s.candidatesScored()

	// 按健康度底线分组（健康度已叠加恢复期惩罚）
	healthy := make([]Scored, 0, len(scored))
	unhealthy := make([]Scored, 0)
	for _, sc := range scored {
		if s.healthOf(sc) >= healthThreshold {
			healthy = append(healthy, sc)
		} else {
			unhealthy = append(unhealthy, sc)
		}
	}

	// 健康线路按就近+健康度排序
	sortScoredByKey(healthy, func(sc Scored) float64 {
		var proximity float64
		if d := lineDistance(sc.Metrics.Line, plat, plon); d != math.MaxFloat64 {
			proximity = clamp01(1 - d/refMaxDist)
		}
		return playerGeoWeight*proximity + (1-playerGeoWeight)*(s.healthOf(sc)/100)
	})
	// 差线路内部按健康度排序（成功率高的优先）
	sortScoredByKey(unhealthy, func(sc Scored) float64 {
		return s.healthOf(sc)
	})

	return linesOf(append(healthy, unhealthy...))
}

// healthOf 返回线路健康度 [0,100]，叠加恢复期惩罚与负载惩罚系数。
// 连续失败后恢复的线路会获得评分折扣；窗口内被频繁选中的线路同样降权，
// 促使地理选路也向空闲线路分流。
func (s *Selector) healthOf(sc Scored) float64 {
	return healthScore(sc.Metrics) *
		s.Penalty(sc.Metrics.Line.Name) *
		s.loadFactor(sc.Metrics.Line.Name)
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

// Sticky 返回当前粘性首选线路名（供状态接口展示）。
func (s *Selector) Sticky() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sticky
}

// StableRounds 返回粘性首选连续稳定轮数（供状态接口展示）。
func (s *Selector) StableRounds() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stableRounds
}

// score 对一组指标绝对归一化后加权打分。
// 延迟使用 min*0.4 + median*0.6 混合值，并按 latencyExp 指数曲线映射；
// latencyExp=1 时退化为线性。
// 采用绝对基准（而非线路间相对值）可避免把极小差异放大到满分区间。
func score(metrics []prober.Metrics, w config.Weights, latencyExp float64) []Scored {
	result := make([]Scored, 0, len(metrics))
	bwMax := maxBandwidth(metrics)

	for _, m := range metrics {
		if !m.Reachable {
			result = append(result, Scored{Metrics: m, Score: 0})
			continue
		}

		// 延迟：min*0.4 + median*0.6 混合，既反映最优能力又抗偶发尖峰。
		mixedLat := mixedLatency(m)
		latScore := latencyScore(mixedLat, float64(refLatency), latencyExp)

		// 稳定性：成功率为主(0.7)，抖动为辅(0.3)。
		jitScore := invRef(float64(m.Jitter), float64(refJitter))
		stabScore := 0.7*m.SuccessRate + 0.3*jitScore

		// 带宽：仅在调用方真正提供了带宽数据时参与评分。
		var bwScore float64
		if bwMax > 0 {
			if m.Bandwidth <= 0 {
				bwScore = 0.5
			} else {
				bwScore = clamp01(m.Bandwidth / bwMax)
			}
		}

		activeWeight := w.Latency + w.Stability
		total := w.Latency*latScore + w.Stability*stabScore
		if bwMax > 0 {
			activeWeight += w.Bandwidth
			total += w.Bandwidth * bwScore
		}
		if activeWeight > 0 {
			total /= activeWeight
		} else {
			total = 0.5
		}
		result = append(result, Scored{Metrics: m, Score: total * 100})
	}
	return result
}

// latencyScore 将延迟（ns）相对基准 ref（ns）按指数曲线映射到 [0,1]。
// 曲线为 1 - (v/ref)^exp。
// exp>1 时低延迟段差异被压缩（避免无关紧要的毫秒级差异主导选路）、
// 高延迟段差异被放大（更重地惩罚烂线路）；exp=1 时退化为线性。
func latencyScore(v, ref, exp float64) float64 {
	if v < 0 {
		v = 0
	}
	if ref <= 0 {
		return 0
	}
	ratio := v / ref
	if ratio > 1 {
		ratio = 1
	}
	return 1 - math.Pow(ratio, exp)
}

// effectiveMinLatency 兼容未提供最小延迟的指标，避免把缺失值当作零延迟。
func effectiveMinLatency(m prober.Metrics) time.Duration {
	if m.MinLatency <= 0 {
		return m.AvgLatency
	}
	return m.MinLatency
}

// effectiveMedianLatency 兼容未提供中位延迟的指标，避免把缺失值当作零延迟。
func effectiveMedianLatency(m prober.Metrics) time.Duration {
	if m.MedianLatency <= 0 {
		return m.AvgLatency
	}
	return m.MedianLatency
}

// mixedLatency 返回参与延迟评分的混合延迟：min*0.4 + median*0.6。
// 用中位数替代均值作为主体，避免偶发尖峰（如一次采样网络拥塞）拉高评分偏差；
// min 反映线路的最优能力。两者都缺失时回退到 AvgLatency。
func mixedLatency(m prober.Metrics) float64 {
	return 0.4*float64(effectiveMinLatency(m)) + 0.6*float64(effectiveMedianLatency(m))
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
