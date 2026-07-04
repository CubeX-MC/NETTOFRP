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
	"nettofrp/internal/mcproto"
	"nettofrp/internal/selector"
)

// Resolver 将线路解析为可直接连接的 host:port。
type Resolver interface {
	Resolve(line config.Line) (string, error)
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
}

// New 创建 auto 反向代理。
func New(cfg *config.Config, sel *selector.Selector, r Resolver) *Proxy {
	return &Proxy{
		listenAddr:       cfg.Listen,
		sel:              sel,
		resolver:         r,
		dialTO:           cfg.ProbeTimeoutDuration(),
		enableTransfer:   cfg.EnableTransfer,
		transferPacketID: int32(cfg.TransferPacketID),
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
	hs, err := mcproto.ReadHandshake(br)
	if err != nil {
		log.Printf("[proxy] 读取握手失败，关闭 %s: %v", client.RemoteAddr(), err)
		return
	}

	// 仅对「开启 Transfer + 登录意图 + 客户端支持 Transfer」的连接走直连优化；
	// 其余（状态查询、旧版本客户端、开关关闭）一律回落纯 TCP 透传。
	eligible := p.enableTransfer &&
		hs.NextState == mcproto.StateLogin &&
		hs.ProtocolVersion >= mcproto.TransferMinProtocol

	candidates := p.sel.Candidates()

	if eligible && len(candidates) > 0 {
		if p.tryTransfer(client, br, hs, candidates) {
			return
		}
		// tryTransfer 内部一旦已向客户端写入协议数据便无法安全回落，
		// 返回 false 仅发生在「尚未写入任何数据」的早期失败，此时可安全回落。
	}

	p.fallbackTCP(client, br, hs, candidates)
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
