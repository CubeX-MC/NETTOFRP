package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/prober"
	"nettofrp/internal/resolver"
	"nettofrp/internal/selector"
	"nettofrp/internal/proxy"
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

	res := resolver.New(cfg)
	pb := prober.New(cfg, res)
	sel := selector.New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 首轮同步探测，确保代理启动时已有可用的最优线路。
	runProbe(cfg, pb, sel)
	go probeLoop(ctx, cfg, pb, sel)

	px := proxy.New(cfg, sel, res)

	go handleSignals(cancel)

	if err := px.Serve(); err != nil {
		log.Fatalf("代理服务退出: %v", err)
	}
}

// probeLoop 按配置周期反复探测所有线路并刷新评分。
func probeLoop(ctx context.Context, cfg *config.Config, pb *prober.Prober, sel *selector.Selector) {
	ticker := time.NewTicker(cfg.ProbeIntervalDuration())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProbe(cfg, pb, sel)
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
		b.WriteString(formatScore(m.Line.Name, sc.Score, m.AvgLatency, m.SuccessRate))
	}
	log.Println(b.String())
}

func formatScore(name string, score float64, lat time.Duration, sr float64) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("=")
	b.WriteString(strconv.FormatFloat(score, 'f', 1, 64))
	b.WriteString("分(")
	b.WriteString(lat.Round(time.Millisecond).String())
	b.WriteString(",成功率")
	b.WriteString(strconv.FormatFloat(sr*100, 'f', 0, 64))
	b.WriteString("%)")
	return b.String()
}

func handleSignals(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	log.Println("收到退出信号，正在关闭...")
	cancel()
	os.Exit(0)
}
