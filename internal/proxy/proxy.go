package proxy

import (
	"bufio"
	"crypto/md5"
	"io"
	"log"
	"net"
	"strconv"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/geoip"
	"nettofrp/internal/mcproto"
	"nettofrp/internal/selector"
)

// handshakeTimeout 是登录协商阶段（Proxy Protocol 头 + 握手 + 登录起始 +
// Login Acknowledged）必须在其内完成的期限。进入 TCP 透传前会清除该读期限，
// 因此不影响正常游戏长连接，只用于挡住连上不发数据的空转/慢喂连接。
const handshakeTimeout = 10 * time.Second

// Resolver 将线路解析为可直接连接的 host:port。
type Resolver interface {
	Resolve(line config.Line) (string, error)
}

// RegionLocator 将 IP 解析为地理位置（经纬度 + 区域标记）。
// 由 geoip.DB 实现；为 nil 时代理不做地理选路。
type RegionLocator interface {
	Locate(ip net.IP) geoip.Location
}

// Proxy 在监听端口上接受玩家连接，作为多条线路统一的 "auto" 入口。
//
// 对支持 Transfer 的客户端（协议 ≥766，即 1.20.5+），代理在登录阶段直接下发
// Transfer 包，令客户端改连最优线路，此后游戏流量不再经过本代理——消除中转延迟。
// 客户端在被 Transfer 到的线路上完成真正的正版验证（如 limbo）。
// 低版本客户端、状态查询、或 Transfer 关闭时，回落到纯 TCP 透传。
type Proxy struct {
	listenAddr       string
	sel              *selector.Selector
	resolver         Resolver
	dialTO           time.Duration
	enableTransfer   bool
	transferPacketID int32
	enableProxyProto bool
	geo              RegionLocator
}

// New 创建 auto 反向代理。geo 可为 nil，表示不启用地理选路。
func New(cfg *config.Config, sel *selector.Selector, r Resolver, geo RegionLocator) *Proxy {
	return &Proxy{
		listenAddr:       cfg.Listen,
		sel:              sel,
		resolver:         r,
		dialTO:           cfg.ProbeTimeoutDuration(),
		enableTransfer:   cfg.EnableTransfer,
		transferPacketID: int32(cfg.TransferPacketID),
		enableProxyProto: cfg.EnableProxyProtocol,
		geo:              geo,
	}
}

// Serve 启动监听循环，阻塞运行直到监听出错。
func (p *Proxy) Serve() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return err
	}
	log.Printf("[proxy] auto 入口已监听 %s", p.listenAddr)

	for {
		client, err := ln.Accept()
		if err != nil {
			log.Printf("[proxy] accept 失败: %v", err)
			continue
		}
		go p.handle(client)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()

	br := bufio.NewReader(client)

	// 登录协商阶段设读超时：握手/登录起始/Login Acknowledged 都必须在此期限内到齐。
	// 否则一个连上就不发数据（或逐字节慢喂）的连接会永久占用一个 goroutine，
	// 在公网入口上构成廉价的资源耗尽面。进入长连接透传前会清除该期限。
	_ = client.SetReadDeadline(time.Now().Add(handshakeTimeout))

	// Proxy Protocol V1 头（若启用）位于任何 MC 数据之前，须最先解析。
	// 取到真实源 IP 用于地理选路；未启用或无合法头时回落 socket 远端地址。
	realIP := p.clientIP(client, br)

	hs, err := mcproto.ReadHandshake(br)
	if err != nil {
		log.Printf("[proxy] 读取握手失败，关闭 %s: %v", client.RemoteAddr(), err)
		return
	}

	// 依据真实 IP 定位玩家位置，据此对候选线路重排序（就近优先）。
	var loc geoip.Location
	if p.geo != nil && realIP != nil {
		loc = p.geo.Locate(realIP)
	}
	if p.enableProxyProto || p.geo != nil {
		log.Printf("[proxy] 连接来源 %s，真实IP=%s，识别区域=%q，有坐标=%t",
			client.RemoteAddr(), maskIP(realIP), loc.Region, loc.HasCoord)
	}

	// 仅对「开启 Transfer + 登录意图 + 客户端支持 Transfer」的连接走直连优化；
	// 其余（状态查询、旧版本客户端、开关关闭）一律回落纯 TCP 透传。
	eligible := p.enableTransfer &&
		hs.NextState == mcproto.StateLogin &&
		hs.ProtocolVersion >= mcproto.TransferMinProtocol

	// 优先按玩家真实坐标就近选路（不受 prober→线路 延迟干扰）；
	// 取不到坐标时降级到按区域标记选路；无地理信息时退化为全局评分。
	var candidates []config.Line
	switch {
	case loc.HasCoord:
		candidates = p.sel.CandidatesForPlayer(loc.Lat, loc.Lon)
	default:
		candidates = p.sel.CandidatesForRegion(loc.Region)
	}

	if eligible && len(candidates) > 0 {
		if p.tryTransfer(client, br, hs, candidates) {
			return
		}
		// tryTransfer 内部一旦已向客户端写入协议数据便无法安全回落，
		// 返回 false 仅发生在「尚未写入任何数据」的早期失败，此时可安全回落。
	}

	p.fallbackTCP(client, br, hs, candidates)
}

// clientIP 返回玩家真实 IP：启用 Proxy Protocol 且成功解析首行时取其中的源 IP，
// 否则回落到 socket 连接的远端 IP。
func (p *Proxy) clientIP(client net.Conn, br *bufio.Reader) net.IP {
	if p.enableProxyProto {
		if ip := readProxyProtocolV1(br); ip != nil {
			return ip
		}
	}
	if addr, ok := client.RemoteAddr().(*net.TCPAddr); ok {
		return addr.IP
	}
	return nil
}

// maskIP 对真实 IP 做脱敏，仅用于日志：IPv4 隐去中间两段（39.1.2.27 -> 39.*.*.27），
// IPv6 仅保留前两字节，避免完整地址落盘。nil 时返回 "-"。
func maskIP(ip net.IP) string {
	if ip == nil {
		return "-"
	}
	if v4 := ip.To4(); v4 != nil {
		return strconv.Itoa(int(v4[0])) + ".*.*." + strconv.Itoa(int(v4[3]))
	}
	if len(ip) == net.IPv6len {
		return strconv.Itoa(int(ip[0])) + strconv.Itoa(int(ip[1])) + ":*:*"
	}
	return "*"
}

// tryTransfer 执行离线登录并向客户端下发 Transfer 包，令其直连最优线路。
// 返回 true 表示已接管该连接（无论成功或已写入数据）；返回 false 表示尚未写入
// 任何数据、可安全回落到 TCP 透传。
func (p *Proxy) tryTransfer(client net.Conn, br *bufio.Reader, hs mcproto.Handshake, candidates []config.Line) bool {
	// 选出最优可达线路并解析为客户端可直连的 host:port。
	line, host, port, ok := p.resolveTarget(candidates)
	if !ok {
		return false // 无可解析线路，交回落处理
	}

	// 读取登录起始包，拿到玩家名与 UUID（尚未向客户端回写任何数据）。
	ls, err := mcproto.ReadLoginStart(br)
	if err != nil {
		log.Printf("[proxy] 读取登录起始失败，关闭 %s: %v", client.RemoteAddr(), err)
		return true // 已消费登录包，无法回落
	}

	uuid := ls.UUID
	if uuid == ([16]byte{}) {
		uuid = offlineUUID(ls.Name)
	}

	// 离线登录：直接回 Login Success，客户端随后发送 Login Acknowledged 进入
	// configuration 状态。真正的正版验证由被 Transfer 到的线路（limbo）完成。
	success := mcproto.BuildLoginSuccess(hs.ProtocolVersion, uuid, ls.Name)
	if err := mcproto.WritePacket(client, 0x02, success); err != nil {
		log.Printf("[proxy] 发送 Login Success 失败 %s: %v", client.RemoteAddr(), err)
		return true
	}

	// 等待 Login Acknowledged（Login 状态 serverbound 0x03）。
	ack, err := mcproto.ReadPacket(br)
	if err != nil {
		log.Printf("[proxy] 等待 Login Acknowledged 失败 %s: %v", client.RemoteAddr(), err)
		return true
	}
	if ack.ID != 0x03 {
		log.Printf("[proxy] 期望 Login Acknowledged(0x03)，实际 0x%02X，关闭 %s", ack.ID, client.RemoteAddr())
		return true
	}

	// configuration 状态下发 Transfer 包，令客户端改连目标线路。
	transfer := mcproto.BuildTransfer(host, port)
	if err := mcproto.WritePacket(client, p.transferPacketID, transfer); err != nil {
		log.Printf("[proxy] 发送 Transfer 失败 %s: %v", client.RemoteAddr(), err)
		return true
	}

	log.Printf("[proxy] %s 玩家 %q 协议%d -> Transfer 直连 %s(%s:%d)",
		client.RemoteAddr(), ls.Name, hs.ProtocolVersion, line.Name, host, port)
	return true
}

// resolveTarget 从候选线路中选出首个可成功解析的线路，返回其 host 与 port。
func (p *Proxy) resolveTarget(candidates []config.Line) (config.Line, string, uint16, bool) {
	for _, line := range candidates {
		addr, err := p.resolver.Resolve(line)
		if err != nil {
			log.Printf("[proxy] 解析线路 %s(%s) 失败，跳过: %v", line.Name, line.Address, err)
			continue
		}
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			log.Printf("[proxy] 线路 %s 地址 %q 解析 host:port 失败，跳过: %v", line.Name, addr, err)
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			log.Printf("[proxy] 线路 %s 端口 %q 非法，跳过", line.Name, portStr)
			continue
		}
		return line, host, uint16(port), true
	}
	return config.Line{}, "", 0, false
}

// fallbackTCP 以纯 TCP 透传方式服务连接：向上游重放握手及已缓冲字节，然后双向转发。
func (p *Proxy) fallbackTCP(client net.Conn, br *bufio.Reader, hs mcproto.Handshake, candidates []config.Line) {
	if len(candidates) == 0 {
		log.Printf("[proxy] 无可用线路，拒绝 %s", client.RemoteAddr())
		return
	}

	upstream, line, addr := p.dialCandidates(candidates)
	if upstream == nil {
		log.Printf("[proxy] 所有候选线路均连接失败，拒绝 %s", client.RemoteAddr())
		return
	}
	defer upstream.Close()

	// 先把已读取的握手帧重放给上游，使其看到与客户端一致的握手。
	if _, err := upstream.Write(hs.Raw); err != nil {
		log.Printf("[proxy] 向上游重放握手失败 %s: %v", line.Name, err)
		return
	}

	// 透传是长连接，清除协商阶段的读期限，否则游戏进行中会被误判超时断开。
	_ = client.SetReadDeadline(time.Time{})

	log.Printf("[proxy] %s -> %s(%s) [TCP透传]", client.RemoteAddr(), line.Name, addr)
	pipeReader(client, br, upstream)
}

// dialCandidates 按顺序尝试连接候选线路，返回首个成功的连接及其线路信息。
// 全部失败时返回 nil。
func (p *Proxy) dialCandidates(candidates []config.Line) (net.Conn, config.Line, string) {
	for _, line := range candidates {
		addr, err := p.resolver.Resolve(line)
		if err != nil {
			log.Printf("[proxy] 解析线路 %s(%s) 失败，跳过: %v", line.Name, line.Address, err)
			continue
		}

		upstream, err := net.DialTimeout("tcp", addr, p.dialTO)
		if err != nil {
			log.Printf("[proxy] 连接线路 %s(%s) 失败，尝试下一条: %v", line.Name, addr, err)
			continue
		}
		return upstream, line, addr
	}
	return nil, config.Line{}, ""
}

// pipeReader 在客户端与上游之间双向转发。客户端方向从 br 读取，
// 以保留握手之后可能已被缓冲的字节；任一方向结束即收敛。
func pipeReader(client net.Conn, br *bufio.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(upstream, br)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	<-done
}

// closeWrite 半关闭写方向，通知对端数据发送完毕。
func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// offlineUUID 依据离线模式规则由玩家名生成 UUID：
// version-3 (MD5) of "OfflinePlayer:<name>"。
func offlineUUID(name string) [16]byte {
	h := md5.Sum([]byte("OfflinePlayer:" + name))
	h[6] = (h[6] & 0x0f) | 0x30 // version 3
	h[8] = (h[8] & 0x3f) | 0x80 // variant RFC 4122
	return h
}
