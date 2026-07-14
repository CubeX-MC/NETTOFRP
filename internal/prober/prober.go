package prober

import (
	"math"
	"net"
	"time"

	"nettofrp/internal/config"
)

// Metrics 保存某条线路一次探测周期采集到的原始网络指标。
type Metrics struct {
	Line        config.Line
	Reachable   bool
	AvgLatency  time.Duration // 平均 TCP 建连延迟
	MinLatency  time.Duration // 最小 TCP 建连延迟
	Jitter      time.Duration // 延迟抖动（标准差）
	SuccessRate float64       // 建连成功率 [0,1]
	Bandwidth   float64       // 估算带宽 (bytes/s)，无法测得时为 0
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
func (p *Prober) Probe(line config.Line) Metrics {
	addr, err := p.resolver.Resolve(line)
	if err != nil {
		// 解析失败（如 SRV 记录查询失败）视为该线路不可达。
		return Metrics{Line: line, Reachable: false}
	}

	latencies := make([]time.Duration, 0, p.samples)
	var success int

	for i := 0; i < p.samples; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, p.timeout)
		if err != nil {
			continue
		}
		latency := time.Since(start)
		latencies = append(latencies, latency)
		success++
		_ = conn.Close()
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
	m.Jitter = stddev(latencies, m.AvgLatency)
	m.Bandwidth = p.measureBandwidth(addr)
	return m
}

// measureBandwidth 通过短时读取估算线路吞吐量。
// 若线路在窗口期内不主动下发数据，返回 0（视为不可测，交由评分层作中性处理）。
func (p *Prober) measureBandwidth(addr string) float64 {
	conn, err := net.DialTimeout("tcp", addr, p.timeout)
	if err != nil {
		return 0
	}
	defer conn.Close()

	window := 300 * time.Millisecond
	_ = conn.SetReadDeadline(time.Now().Add(window))

	buf := make([]byte, 32*1024)
	var total int
	start := time.Now()
	for {
		n, err := conn.Read(buf)
		total += n
		if err != nil {
			break
		}
		if time.Since(start) >= window {
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 || total == 0 {
		return 0
	}
	return float64(total) / elapsed
}

func mean(ds []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

func stddev(ds []time.Duration, avg time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	var sq float64
	for _, d := range ds {
		diff := float64(d - avg)
		sq += diff * diff
	}
	return time.Duration(math.Sqrt(sq / float64(len(ds))))
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
