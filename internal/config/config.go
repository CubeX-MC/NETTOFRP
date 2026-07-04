package config

import (
	"encoding/json"
	"fmt"
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

	// EnableTransfer 为 true 时，对支持 Transfer 的客户端（协议 ≥766，即 1.20.5+）
	// 在登录阶段直接下发 Transfer 包，令其直连最优线路，游戏流量不经过本代理。
	// 低版本客户端或该开关关闭时，回落到纯 TCP 转发。
	EnableTransfer bool `json:"enable_transfer"`

	// TransferPacketID 是 configuration 状态下 Transfer 包的 ID。
	// 1.20.5~1.21.x 为 0x0B(11)。未来版本若包 ID 变动，可在此覆盖而无需改代码。
	TransferPacketID int `json:"transfer_packet_id"`
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
	for i, l := range c.Lines {
		if l.Name == "" {
			return fmt.Errorf("第 %d 条线路缺少 name", i+1)
		}
		if l.Address == "" {
			return fmt.Errorf("线路 %q 缺少 address", l.Name)
		}
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
		c.TransferPacketID = 0x0B // 1.20.5~1.21.x configuration 状态 Transfer 包 ID
	}
	w := c.Weights
	if w.Latency == 0 && w.Stability == 0 && w.Bandwidth == 0 {
		c.Weights = Weights{Latency: 0.6, Stability: 0.3, Bandwidth: 0.1}
	}
}
