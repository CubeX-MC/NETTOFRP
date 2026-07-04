package resolver

import (
	"errors"
	"testing"
	"time"

	"nettofrp/internal/config"
)

func newResolver() *Resolver {
	return New(&config.Config{ProbeTimeout: 1000})
}

// 非 SRV 线路应直接返回其配置地址，不触发任何查询。
func TestResolveNonSRV(t *testing.T) {
	r := newResolver()
	r.lookup = func(string) (string, error) {
		t.Fatal("非 SRV 线路不应调用 lookup")
		return "", nil
	}

	addr, err := r.Resolve(config.Line{Name: "a", Address: "host:25565"})
	if err != nil || addr != "host:25565" {
		t.Fatalf("期望 host:25565，实际 addr=%q err=%v", addr, err)
	}
}

// SRV 查询失败但曾解析成功过时，应回退到上次的结果。
func TestResolveSRVStaleFallback(t *testing.T) {
	r := newResolver()
	line := config.Line{Name: "srv", Address: "play.example.org", SRV: true}

	// 首次查询成功，写入缓存。
	r.lookup = func(string) (string, error) { return "real.host:3503", nil }
	addr, err := r.Resolve(line)
	if err != nil || addr != "real.host:3503" {
		t.Fatalf("首次解析期望 real.host:3503，实际 addr=%q err=%v", addr, err)
	}

	// 让缓存过期，并模拟 DNS 抖动失败。
	r.mu.Lock()
	e := r.cache[line.Address]
	e.expires = time.Now().Add(-time.Second)
	r.cache[line.Address] = e
	r.mu.Unlock()
	r.lookup = func(string) (string, error) { return "", errors.New("dns timeout") }

	addr, err = r.Resolve(line)
	if err != nil {
		t.Fatalf("DNS 失败时应回退不报错，实际 err=%v", err)
	}
	if addr != "real.host:3503" {
		t.Fatalf("期望回退到 real.host:3503，实际 %q", addr)
	}
}

// 从未解析成功过时，SRV 查询失败应返回错误。
func TestResolveSRVNoCacheError(t *testing.T) {
	r := newResolver()
	r.lookup = func(string) (string, error) { return "", errors.New("dns timeout") }

	_, err := r.Resolve(config.Line{Name: "srv", Address: "play.example.org", SRV: true})
	if err == nil {
		t.Fatal("无缓存且查询失败时应返回错误")
	}
}
