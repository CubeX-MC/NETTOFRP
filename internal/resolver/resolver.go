package resolver

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"nettofrp/internal/config"
)

// ttl 是 SRV 解析结果的缓存有效期，避免每次探测/连接都打 DNS。
const ttl = 30 * time.Second

type entry struct {
	addr    string // 解析出的真实 host:port
	expires time.Time
}

// Resolver 将线路配置解析为可直接 Dial 的 host:port。
// 非 SRV 线路直接返回其 Address；SRV 线路查询 _minecraft._tcp.<域名>。
type Resolver struct {
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]entry

	// lookup 查询域名的 SRV 记录，返回真实 host:port。
	// 默认走真实 DNS，测试可替换以模拟 DNS 抖动。
	lookup func(domain string) (string, error)
}

// New 创建解析器。
func New(cfg *config.Config) *Resolver {
	r := &Resolver{
		timeout: cfg.ProbeTimeoutDuration(),
		cache:   make(map[string]entry),
	}
	r.lookup = r.lookupSRV
	return r
}

// Resolve 返回线路可直接连接的 host:port。
// SRV 线路优先用未过期的缓存；缓存过期则重新查询，
// 若查询失败但曾解析成功过，则回退到上次的结果，避免 DNS 抖动拖慢连接。
func (r *Resolver) Resolve(line config.Line) (string, error) {
	if !line.SRV {
		return line.Address, nil
	}

	r.mu.Lock()
	cached, hasCache := r.cache[line.Address]
	r.mu.Unlock()

	if hasCache && time.Now().Before(cached.expires) {
		return cached.addr, nil
	}

	addr, err := r.lookup(line.Address)
	if err != nil {
		if hasCache {
			// DNS 临时失败，回退到上次成功解析的结果。
			log.Printf("[resolver] SRV 查询 %q 失败，回退到上次结果 %s: %v", line.Address, cached.addr, err)
			return cached.addr, nil
		}
		return "", err
	}

	r.mu.Lock()
	r.cache[line.Address] = entry{addr: addr, expires: time.Now().Add(ttl)}
	r.mu.Unlock()
	return addr, nil
}

// lookupSRV 查询 _minecraft._tcp.<domain> 并返回优先级最高的目标 host:port。
func (r *Resolver) lookupSRV(domain string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	_, records, err := net.DefaultResolver.LookupSRV(ctx, "minecraft", "tcp", domain)
	if err != nil {
		return "", fmt.Errorf("查询 SRV 记录 %q 失败: %w", domain, err)
	}
	if len(records) == 0 {
		return "", fmt.Errorf("域名 %q 无 SRV 记录", domain)
	}

	// LookupSRV 已按优先级/权重排序，取首条。Target 末尾带 '.'，去掉。
	best := records[0]
	host := best.Target
	if n := len(host); n > 0 && host[n-1] == '.' {
		host = host[:n-1]
	}
	return net.JoinHostPort(host, strconv.Itoa(int(best.Port))), nil
}
