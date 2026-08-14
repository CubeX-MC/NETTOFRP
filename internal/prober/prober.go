package prober

import (
	"net"
	"sort"
	"sync"
	"time"

	"nettofrp/internal/config"
)

// madScale 是 MAD 与标准差的标度换算常数：正态分布下 stddev ≈ 1.4826 × MAD。
// 乘以该常数后抖动数值标度与原标准差一致，抗尖峰能力更强。
const madScale = 1.4826

// Metrics 保存某条线路一次探测周期采集到的原始网络指标。
type Metrics struct {
	Line          config.Line
	Reachable     bool
	AvgLatency    time.Duration // 平均 TCP 建连延迟
	MinLatency    time.Duration // 最小 TCP 建连延迟
	MedianLatency time.Duration // 中位 TCP 建连延迟（P50，抗偶发尖峰）
	Jitter        time.Duration // 延迟抖动（MAD×1.4826，抗尖峰）
	SuccessRate   float64       // 建连成功率 [0,1]
	Bandwidth     float64       // 保留字段；当前 TCP 探测不伪造带宽数据，恒为 0
}

// Resolver 将线路解析为可直接连接的 host:port。
type Resolver interface {
	Resolve(line config.Line) (string, error)
}

// Prober 负责对单条线路执行网络质量采集。
type Prober struct {
	samples  int
	timeout  time.Duration
	resolver Resolver
}

// New 创建一个探测器。
func New(cfg *config.Config, r Resolver) *Prober {
	return &Prober{
		samples:  cfg.ProbeSamples,
		timeout:  cfg.ProbeTimeoutDuration(),
		resolver: r,
	}
}

// Probe 对一条线路进行多次采样，返回聚合后的指标。
// 采样并行发出：整轮探测最坏耗时从 N×timeout（串行超时累加）降为单个 timeout，
// 对不可达线路尤为关键——否则一条死线路会让整个探测循环阻塞数秒。
func (p *Prober) Probe(line config.Line) Metrics {
	addr, err := p.resolver.Resolve(line)
	if err != nil {
		// 解析失败（如 SRV 记录查询失败）视为该线路不可达。
		return Metrics{Line: line, Reachable: false}
	}

	type sample struct {
		latency time.Duration
		ok      bool
	}
	results := make(chan sample, p.samples)
	var wg sync.WaitGroup
	for i := 0; i < p.samples; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			conn, err := net.DialTimeout("tcp", addr, p.timeout)
			if err != nil {
				results <- sample{ok: false}
				return
			}
			latency := time.Since(start)
			_ = conn.Close()
			results <- sample{latency: latency, ok: true}
		}()
	}
	wg.Wait()
	close(results)

	latencies := make([]time.Duration, 0, p.samples)
	var success int
	for s := range results {
		if s.ok {
			latencies = append(latencies, s.latency)
			success++
		}
	}

	m := Metrics{
		Line:        line,
		SuccessRate: float64(success) / float64(p.samples),
		Reachable:   success > 0,
	}
	if len(latencies) == 0 {
		return m
	}

	m.AvgLatency = mean(latencies)
	m.MinLatency = minDuration(latencies)
	m.MedianLatency = medianDuration(latencies)
	m.Jitter = mad(latencies, m.MedianLatency)
	return m
}

func mean(ds []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

// mad 返回延迟序列的中位绝对偏差（MAD）：median(|x - median(x)|)，乘以 madScale
// 使数值标度与原标准差一致。与标准差相比，MAD 对个别尖峰（如一次网络拥塞导致的
// 500ms 样本）更稳健，不会被单个离群点拉高。
func mad(ds []time.Duration, med time.Duration) time.Duration {
	n := len(ds)
	if n < 2 {
		return 0
	}
	devs := make([]time.Duration, n)
	for i, d := range ds {
		if d > med {
			devs[i] = d - med
		} else {
			devs[i] = med - d
		}
	}
	return time.Duration(madScale * float64(medianDuration(devs)))
}

func minDuration(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds[1:] {
		if d < m {
			m = d
		}
	}
	return m
}

// medianDuration 返回中位延迟（P50）。样本为奇数取中间值，偶数取中间两值平均。
// 对原始切片做副本排序，不修改输入。
func medianDuration(ds []time.Duration) time.Duration {
	n := len(ds)
	if n == 0 {
		return 0
	}
	sorted := make([]time.Duration, n)
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
