package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"math"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/geoip"
	"nettofrp/internal/prober"
	"nettofrp/internal/proxy"
	"nettofrp/internal/resolver"
	"nettofrp/internal/selector"
)

// version 在发布构建时由 -ldflags "-X main.version=..." 注入，本地构建为 dev。
var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	log.Printf("[main] NETTOFRP %s 启动", version)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg.MCHost != "" {
		log.Printf("[main] 警告: mc_host 仅为兼容旧配置保留，当前不参与路由，可从配置中删除")
	}

	res := resolver.New(cfg)
	pb := prober.New(cfg, res)
	sel := selector.New(cfg)

	// 可选地理选路：配置了 geoip_db 且加载成功时启用，否则以 nil 传入（不做地理选路）。
	var geo *geoip.DB
	if cfg.GeoIPDB != "" {
		g, err := geoip.Open(cfg.GeoIPDB)
		if err != nil {
			log.Fatalf("加载 GeoIP 数据库失败: %v", err)
		}
		geo = g
		defer geo.Close()
		log.Printf("[main] 已启用地理选路，GeoIP 库: %s", cfg.GeoIPDB)
	}

	// proxy.New 接受 RegionLocator 接口；geo 为 nil 时需显式传 nil 接口值。
	var locator proxy.RegionLocator
	if geo != nil {
		locator = geo
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 首轮同步探测，确保代理启动时已有可用的最优线路。
	runProbe(cfg, pb, sel)
	go probeLoop(ctx, cfg, pb, sel)

	// 可选只读状态接口：便于运维监控各线路评分与首选。
	if cfg.StatusListen != "" {
		startStatusServer(ctx, cfg.StatusListen, sel)
	}

	px := proxy.New(cfg, sel, res, locator)

	if err := px.Serve(ctx); err != nil {
		log.Fatalf("代理服务退出: %v", err)
	}
	log.Println("代理服务已关闭")
}

// probeLoop 按自适应周期反复探测所有线路并刷新评分。
// 线路稳定时拉长间隔降低无谓探测，出现故障/切换时缩短间隔尽快收敛。
func probeLoop(ctx context.Context, cfg *config.Config, pb *prober.Prober, sel *selector.Selector) {
	interval := cfg.ProbeIntervalDuration()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProbe(cfg, pb, sel)
			// 根据本轮稳定性调整下一轮间隔（Reset 丢弃积压 tick，重新计时）。
			if next := sel.RecommendedInterval(cfg.ProbeIntervalDuration()); next != interval {
				interval = next
				ticker.Reset(interval)
				log.Printf("[probe] 探测间隔调整为 %s（线路状态变化）", interval)
			}
		}
	}
}

// runProbe 并行探测全部线路，聚合后更新选择器。
func runProbe(cfg *config.Config, pb *prober.Prober, sel *selector.Selector) {
	metrics := make([]prober.Metrics, len(cfg.Lines))
	var wg sync.WaitGroup
	for i, line := range cfg.Lines {
		wg.Add(1)
		go func(idx int, l config.Line) {
			defer wg.Done()
			metrics[idx] = pb.Probe(l)
		}(i, line)
	}
	wg.Wait()

	sel.Update(metrics)
	logRanking(sel)
}

func logRanking(sel *selector.Selector) {
	var b strings.Builder
	b.WriteString("[probe] 线路评分: ")
	for i, sc := range sel.Ranking() {
		if i > 0 {
			b.WriteString(" | ")
		}
		m := sc.Metrics
		if !m.Reachable {
			b.WriteString(m.Line.Name + "=不可达")
			continue
		}
		b.WriteString(formatScore(m.Line.Name, sc.Score, m.MinLatency, m.MedianLatency, m.AvgLatency, m.SuccessRate))
	}
	log.Println(b.String())
}

func formatScore(name string, score float64, min, med, avg time.Duration, sr float64) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("=")
	b.WriteString(strconv.FormatFloat(score, 'f', 1, 64))
	b.WriteString("分(min:")
	b.WriteString(min.Round(time.Millisecond).String())
	b.WriteString(" med:")
	b.WriteString(med.Round(time.Millisecond).String())
	b.WriteString(" avg:")
	b.WriteString(avg.Round(time.Millisecond).String())
	b.WriteString(" 成功率:")
	b.WriteString(strconv.FormatFloat(sr*100, 'f', 0, 64))
	b.WriteString("%)")
	return b.String()
}

// startStatusServer 启动只读状态 HTTP 接口，随 ctx 取消优雅关闭。
func startStatusServer(ctx context.Context, listenAddr string, sel *selector.Selector) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeStatusJSON(w, sel)
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		log.Printf("[status] 状态接口已监听 %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[status] 状态接口退出: %v", err)
		}
	}()
}

// writeStatusJSON 输出当前各线路评分、延迟、成功率与首选标记。
func writeStatusJSON(w http.ResponseWriter, sel *selector.Selector) {
	type lineStatus struct {
		Name        string  `json:"name"`
		Reachable   bool    `json:"reachable"`
		Score       float64 `json:"score"`
		MinLatency  string  `json:"min_latency"`
		MedianLatency string `json:"median_latency"`
		AvgLatency  string  `json:"avg_latency"`
		Jitter      string  `json:"jitter"`
		SuccessRate float64 `json:"success_rate"`
		Sticky      bool    `json:"sticky"`
	}

	ranking := sel.Ranking()
	sticky := sel.Sticky()
	lines := make([]lineStatus, 0, len(ranking))
	for _, sc := range ranking {
		ls := lineStatus{
			Name:        sc.Metrics.Line.Name,
			Reachable:   sc.Metrics.Reachable,
			Score:       mathRound(sc.Score, 1),
			MinLatency:  sc.Metrics.MinLatency.Round(time.Millisecond).String(),
			MedianLatency: sc.Metrics.MedianLatency.Round(time.Millisecond).String(),
			AvgLatency:  sc.Metrics.AvgLatency.Round(time.Millisecond).String(),
			Jitter:      sc.Metrics.Jitter.Round(time.Millisecond).String(),
			SuccessRate: mathRound(sc.Metrics.SuccessRate, 3),
			Sticky:      sc.Metrics.Line.Name == sticky,
		}
		lines = append(lines, ls)
	}

	resp := map[string]any{
		"version":       version,
		"sticky":        sticky,
		"stable_rounds": sel.StableRounds(),
		"lines":         lines,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func mathRound(v float64, decimals int) float64 {
	pow := math.Pow(10, float64(decimals))
	return math.Round(v*pow) / pow
}
