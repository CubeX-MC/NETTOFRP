package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseCfg() Config {
	return Config{
		Listen: ":25565",
		Lines:  []Line{{Name: "a", Address: "127.0.0.1:25566"}},
	}
}

func TestValidateRejectsNegativeWeight(t *testing.T) {
	c := baseCfg()
	c.Weights = Weights{Latency: -0.1, Stability: 0.5, Bandwidth: 0.4}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "权重") {
		t.Fatalf("负权重应被拒绝，实际: %v", err)
	}
}

func TestValidateRejectsDuplicateLineName(t *testing.T) {
	c := baseCfg()
	c.Lines = append(c.Lines, Line{Name: "a", Address: "127.0.0.1:25567"})
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复线路名应被拒绝，实际: %v", err)
	}
}

func TestApplyDefaultsNormalizesWeights(t *testing.T) {
	c := baseCfg()
	c.Weights = Weights{Latency: 3, Stability: 6, Bandwidth: 1}
	c.applyDefaults()
	sum := c.Weights.Latency + c.Weights.Stability + c.Weights.Bandwidth
	if sum < 0.9999 || sum > 1.0001 {
		t.Fatalf("权重归一化后总和应为 1，实际 %f", sum)
	}
	if c.Weights.Latency < 0.2999 || c.Weights.Latency > 0.3001 {
		t.Fatalf("Latency 期望 0.3，实际 %f", c.Weights.Latency)
	}
}

func TestApplyDefaultsZeroWeightUsesDefault(t *testing.T) {
	c := baseCfg()
	c.applyDefaults()
	if c.Weights.Latency != 0.65 || c.Weights.Stability != 0.35 || c.Weights.Bandwidth != 0 {
		t.Fatalf("零权重应使用默认值，实际 %+v", c.Weights)
	}
}

func TestValidateRejectsSRVAddressWithPort(t *testing.T) {
	c := baseCfg()
	c.Lines[0] = Line{Name: "srv", Address: "play.example.org:25565", SRV: true}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "srv=true") {
		t.Fatalf("SRV 地址带端口应被拒绝，实际: %v", err)
	}
}

func TestValidateRejectsDirectAddressWithoutPort(t *testing.T) {
	c := baseCfg()
	c.Lines[0].Address = "play.example.org"
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("直连地址缺少端口应被拒绝，实际: %v", err)
	}
}

func TestValidateRejectsInvalidRegionCode(t *testing.T) {
	c := baseCfg()
	c.Lines[0].Regions = []string{"cn-zj"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "区域代码") {
		t.Fatalf("非法区域代码应被拒绝，实际: %v", err)
	}
}

func TestValidateRejectsNegativeTransferPacketID(t *testing.T) {
	c := baseCfg()
	c.TransferPacketID = -1
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "transfer_packet_id") {
		t.Fatalf("负包 ID 应被拒绝，实际: %v", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"listen":":25565","lines":[{"name":"a","address":"127.0.0.1:25566"}],"probe_sampels":5}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("未知字段应被拒绝，实际: %v", err)
	}
}

func TestValidateAcceptsSRVDomainWithoutPort(t *testing.T) {
	c := baseCfg()
	c.Lines[0] = Line{Name: "srv", Address: "play.example.org", SRV: true, Regions: []string{"JP"}}
	if err := c.validate(); err != nil {
		t.Fatalf("合法 SRV 配置应通过，实际: %v", err)
	}
}

func TestValidateRejectsInvalidTrustedProxyCIDR(t *testing.T) {
	c := baseCfg()
	c.ProxyProtocolTrustedCIDRs = []string{"not-a-cidr"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "proxy_protocol_trusted_cidrs") {
		t.Fatalf("非法可信代理网段应被拒绝，实际: %v", err)
	}
}

func TestValidateRejectsNegativeLatencyExponent(t *testing.T) {
	c := baseCfg()
	c.LatencyScoreExponent = -1
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "latency_score_exponent") {
		t.Fatalf("负指数应被拒绝，实际: %v", err)
	}
}

func TestApplyDefaultsLatencyExponent(t *testing.T) {
	c := baseCfg()
	c.applyDefaults()
	if c.LatencyScoreExponent != 2.0 {
		t.Fatalf("指数默认值应为 2.0，实际 %f", c.LatencyScoreExponent)
	}
}

func TestApplyDefaultsPreservesCustomLatencyExponent(t *testing.T) {
	c := baseCfg()
	c.LatencyScoreExponent = 1.5
	c.applyDefaults()
	if c.LatencyScoreExponent != 1.5 {
		t.Fatalf("自定义指数应保留，实际 %f", c.LatencyScoreExponent)
	}
}

func TestEffectiveProxyProtocolTrustedCIDRsDefaultsToLoopback(t *testing.T) {
	c := baseCfg()
	got := c.EffectiveProxyProtocolTrustedCIDRs()
	if len(got) != 2 || got[0] != "127.0.0.0/8" || got[1] != "::1/128" {
		t.Fatalf("默认应只信任回环地址，实际 %v", got)
	}
}

func TestConfigExampleLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	if _, err := Load(path); err != nil {
		t.Fatalf("config.example.json 应通过严格校验，实际: %v", err)
	}
}
