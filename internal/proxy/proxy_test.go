package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"nettofrp/internal/config"
	"nettofrp/internal/mcproto"
	"nettofrp/internal/prober"
	"nettofrp/internal/resolver"
	"nettofrp/internal/selector"
)

// startEcho 启动一个本地回显后端，返回其地址与关闭函数。
func startEcho(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				io.Copy(conn, conn)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// writeHandshake 向 conn 写入一个握手包（包 ID 0x00）。
func writeHandshake(t *testing.T, conn net.Conn, proto int32, addr string, port uint16, next int32) {
	t.Helper()
	var data []byte
	data = mcproto.AppendVarInt(data, proto)
	data = mcproto.AppendString(data, addr)
	data = binary.BigEndian.AppendUint16(data, port)
	data = mcproto.AppendVarInt(data, next)
	if err := mcproto.WritePacket(conn, 0x00, data); err != nil {
		t.Fatal(err)
	}
}

// startProxy 在随机端口启动被测代理，返回监听地址与关闭函数。
func startProxy(t *testing.T, px *Proxy) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go px.handle(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// 低版本客户端（协议 <766）应回落纯 TCP 透传：
// 代理向上游重放握手，随后双向转发，回显后端原样返回全部字节。
func TestProxyFallbackTCP(t *testing.T) {
	backAddr, closeBack := startEcho(t)
	defer closeBack()

	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		ProbeTimeout:   1000,
		EnableTransfer: true, // 即便开启，低版本也应回落
		Weights:        config.Weights{Latency: 0.6, Stability: 0.3, Bandwidth: 0.1},
		Lines:          []config.Line{{Name: "play1", Address: backAddr}},
	}
	cfg2 := *cfg
	cfg2.TransferPacketID = 0x0B

	sel := selector.New(cfg)
	sel.Update([]prober.Metrics{
		{Line: cfg.Lines[0], Reachable: true, AvgLatency: 10 * time.Millisecond, SuccessRate: 1},
	})

	px := New(&cfg2, sel, resolver.New(&cfg2), nil)
	addr, closeProxy := startProxy(t, px)
	defer closeProxy()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 协议 47（1.8）远低于 Transfer 门槛，强制走透传。
	writeHandshake(t, conn, 47, "auto.example.org", 25565, mcproto.StateLogin)
	payload := []byte("HELLO-AUTO")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}

	// 回显后端会先收到重放的握手帧，再收到 payload，并原样回传两者。
	// 我们只需确认握手帧之后能读到 payload，即证明透传链路通。
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReader(conn)
	hs, err := mcproto.ReadHandshake(br)
	if err != nil {
		t.Fatalf("读回显握手失败: %v", err)
	}
	if hs.ProtocolVersion != 47 || hs.NextState != mcproto.StateLogin {
		t.Fatalf("回显握手内容不符: %+v", hs)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("读回显 payload 失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("透传数据不匹配: 期望 %q 实际 %q", payload, got)
	}
}

// 支持 Transfer 的客户端（协议 ≥766）应收到 Login Success，
// 在发送 Login Acknowledged 后收到指向最优线路的 Transfer 包。
func TestProxyTransfer(t *testing.T) {
	cfg := &config.Config{
		Listen:           "127.0.0.1:0",
		ProbeTimeout:     1000,
		EnableTransfer:   true,
		TransferPacketID: 0x0B,
		Weights:          config.Weights{Latency: 0.6, Stability: 0.3, Bandwidth: 0.1},
		Lines:            []config.Line{{Name: "best", Address: "play.example.org:3503"}},
	}

	sel := selector.New(cfg)
	sel.Update([]prober.Metrics{
		{Line: cfg.Lines[0], Reachable: true, AvgLatency: 10 * time.Millisecond, SuccessRate: 1},
	})

	px := New(cfg, sel, resolver.New(cfg), nil)
	addr, closeProxy := startProxy(t, px)
	defer closeProxy()

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	// 协议 767 = 1.21.1，支持 Transfer 且 Login Success 需带 Strict Error Handling。
	const proto = 767
	writeHandshake(t, conn, proto, "auto.example.org", 25565, mcproto.StateLogin)

	// 登录起始：玩家名 + UUID。
	var ls []byte
	ls = mcproto.AppendString(ls, "Steve")
	ls = append(ls, make([]byte, 16)...) // 零 UUID，触发离线 UUID 生成
	if err := mcproto.WritePacket(conn, 0x00, ls); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(conn)

	// 期望收到 Login Success（0x02）。
	success, err := mcproto.ReadPacket(br)
	if err != nil {
		t.Fatalf("读 Login Success 失败: %v", err)
	}
	if success.ID != 0x02 {
		t.Fatalf("期望 Login Success(0x02)，实际 0x%02X", success.ID)
	}

	// 发送 Login Acknowledged（0x03，无负载）。
	if err := mcproto.WritePacket(conn, 0x03, nil); err != nil {
		t.Fatal(err)
	}

	// 期望收到 Transfer（configuration 0x0B），负载为 host + port。
	transfer, err := mcproto.ReadPacket(br)
	if err != nil {
		t.Fatalf("读 Transfer 失败: %v", err)
	}
	if transfer.ID != 0x0B {
		t.Fatalf("期望 Transfer(0x0B)，实际 0x%02X", transfer.ID)
	}
	host, port := parseTransfer(t, transfer.Data)
	if host != "play.example.org" || port != 3503 {
		t.Fatalf("Transfer 目标不符: %s:%d", host, port)
	}
}

// parseTransfer 解析 Transfer 负载：String host + VarInt port。
func parseTransfer(t *testing.T, data []byte) (string, int32) {
	t.Helper()
	r := bytes.NewReader(data)
	n, err := mcproto.ReadVarInt(r)
	if err != nil {
		t.Fatal(err)
	}
	host := make([]byte, n)
	if _, err := io.ReadFull(r, host); err != nil {
		t.Fatal(err)
	}
	port, err := mcproto.ReadVarInt(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(host), port
}
