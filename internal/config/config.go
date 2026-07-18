package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"
)

// Weights 定义各评估指标在综合评分中的权重。
type Weights struct {
	Latency   float64 `json:"latency"`
	Stability float64 `json:"stability"`
	Bandwidth float64 `json:"bandwidth"`
}

// Line 表示一条独立的 FRP 线路。
type Line struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// SRV 为 true 时，Address 是一个域名（不含端口），
	// 真实的 host:port 需通过查询 _minecraft._tcp.<Address> 的 SRV 记录获得。
	SRV bool `json:"srv"`
	// Regions 是该线路适合服务的区域标记（如 "CN-ZJ"、"CN-GD"、"CN"）。
	// 启用 GeoIP 选路时，玩家所在区域命中其中任一标记的线路会被优先选择。
	// 为空表示不限定区域，可服务任意玩家（作为通用回落线路）。
	Regions []string `json:"regions"`
}

// Config 是程序的完整配置。
type Config struct {
	Listen        string  `json:"listen"`
	MCHost        string  `json:"mc_host"`
	ProbeInterval int     `json:"probe_interval_seconds"`
	ProbeSamples  int     `json:"probe_samples"`
	ProbeTimeout  int     `json:"probe_timeout_ms"`
	Weights       Weights `json:"weights"`
	Lines         []Line  `json:"lines"`

	// EnableTransfer 为 true 时，对已验证的客户端（协议 766~776，即 1.20.5~26.2）
	// 在登录阶段直接下发 Transfer 包，令其直连最优线路，游戏流量不经过本代理。
	// 低版本、未来未知版本或该开关关闭时，回落到纯 TCP 转发。
	EnableTransfer bool `json:"enable_transfer"`

	// TransferPacketID 是 configuration 状态下 Transfer 包的 ID。
	// 1.20.5~26.2 为 0x0B(11)。
	TransferPacketID int `json:"transfer_packet_id"`

	// EnableProxyProtocol 为 true 时，将每个新连接的首行按 Proxy Protocol V1
	// 解析以获取玩家真实源 IP（适用于 NETTOFRP 前置了会发送 PROXY 头的代理，
	// 如 frp 开启 proxy_protocol 或 HAProxy）。读不到合法 PROXY 头时回落使用
	// socket 的远端地址。仅在确有前置代理发送 PROXY 头时开启。
	EnableProxyProtocol bool `json:"enable_proxy_protocol"`

	// GeoIPDB 是 MaxMind GeoLite2-City 数据库(.mmdb)的路径。非空且文件可加载时，
	// 启用基于玩家真实 IP 的地理选路：优先选择 Regions 命中玩家所在区域的线路。
	// 为空则不做地理选路，沿用全局评分排序。
	GeoIPDB string `json:"geoip_db"`
}

// ProbeIntervalDuration 返回探测周期的 time.Duration 形式。
func (c *Config) ProbeIntervalDuration() time.Duration {
	return time.Duration(c.ProbeInterval) * time.Second
}

// ProbeTimeoutDuration 返回单次探测超时的 time.Duration 形式。
func (c *Config) ProbeTimeoutDuration() time.Duration {
	return time.Duration(c.ProbeTimeout) * time.Millisecond
}

// Load 从指定路径读取并校验 JSON 配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen 不能为空")
	}
	if len(c.Lines) == 0 {
		return fmt.Errorf("至少需要配置一条 FRP 线路")
	}
	names := make(map[string]bool, len(c.Lines))
	for i, l := range c.Lines {
		if l.Name == "" {
			return fmt.Errorf("第 %d 条线路缺少 name", i+1)
		}
		if l.Address == "" {
			return fmt.Errorf("线路 %q 缺少 address", l.Name)
		}
		if names[l.Name] {
			return fmt.Errorf("线路名重复: %q", l.Name)
		}
		names[l.Name] = true
	}
	if err := c.validateWeights(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateWeights() error {
	w := c.Weights
	// 全零时 applyDefaults 会填默认值，此处跳过
	if w.Latency == 0 && w.Stability == 0 && w.Bandwidth == 0 {
		return nil
	}
	for _, v := range [3]float64{w.Latency, w.Stability, w.Bandwidth} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return fmt.Errorf("权重必须为非负有限数（latency=%.3f stability=%.3f bandwidth=%.3f）",
				w.Latency, w.Stability, w.Bandwidth)
		}
	}
	sum := w.Latency + w.Stability + w.Bandwidth
	if sum <= 0 {
		return fmt.Errorf("权重之和必须大于零")
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.ProbeInterval <= 0 {
		c.ProbeInterval = 15
	}
	if c.ProbeSamples <= 0 {
		c.ProbeSamples = 5
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 2000
	}
	if c.TransferPacketID == 0 {
		c.TransferPacketID = 0x0B // 1.20.5~26.2 configuration 状态 Transfer 包 ID
	}
	w := c.Weights
	if w.Latency == 0 && w.Stability == 0 && w.Bandwidth == 0 {
		c.Weights = Weights{Latency: 0.6, Stability: 0.3, Bandwidth: 0.1}
	} else {
		// 归一化，确保总和 = 1
		sum := w.Latency + w.Stability + w.Bandwidth
		c.Weights = Weights{
			Latency:   w.Latency / sum,
			Stability: w.Stability / sum,
			Bandwidth: w.Bandwidth / sum,
		}
	}
}
