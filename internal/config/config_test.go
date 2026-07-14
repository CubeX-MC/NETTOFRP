package config

import (
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
	if c.Weights.Latency != 0.6 || c.Weights.Stability != 0.3 || c.Weights.Bandwidth != 0.1 {
		t.Fatalf("零权重应使用默认值，实际 %+v", c.Weights)
	}
}
