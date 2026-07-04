package proxy

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// 合法的 TCP4 PROXY 头应被解析出源 IP，且头行被消费、后续 MC 数据完整保留。
func TestReadProxyProtocolV1TCP4(t *testing.T) {
	payload := "PROXY TCP4 203.0.113.7 198.51.100.1 56324 25565\r\n\x00\x10rest-of-handshake"
	br := bufio.NewReader(strings.NewReader(payload))

	ip := readProxyProtocolV1(br)
	if ip == nil || ip.String() != "203.0.113.7" {
		t.Fatalf("期望源 IP 203.0.113.7，实际 %v", ip)
	}

	// 头行之后的字节应原样保留给后续 MC 握手解析。
	rest, _ := br.Peek(2)
	if !bytes.Equal(rest, []byte{0x00, 0x10}) {
		t.Fatalf("PROXY 头之后的数据被破坏: %v", rest)
	}
}

// 非 PROXY 头（普通 MC 握手）不应被消费任何字节，返回 nil。
func TestReadProxyProtocolV1NoHeader(t *testing.T) {
	// 典型 MC 握手帧起始字节，绝不以 "PROXY " 开头。
	raw := []byte{0x10, 0x00, 0xf6, 0x05}
	br := bufio.NewReader(bytes.NewReader(raw))

	if ip := readProxyProtocolV1(br); ip != nil {
		t.Fatalf("非 PROXY 头应返回 nil，实际 %v", ip)
	}
	// 未消费任何字节：首字节仍是 0x10。
	head, _ := br.Peek(1)
	if len(head) != 1 || head[0] != 0x10 {
		t.Fatalf("非 PROXY 头不应消费缓冲，实际首字节 %v", head)
	}
}

// UNKNOWN 协议头应被消费但返回 nil，交由调用方回落 socket 地址。
func TestReadProxyProtocolV1Unknown(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("PROXY UNKNOWN\r\nnext"))
	if ip := readProxyProtocolV1(br); ip != nil {
		t.Fatalf("UNKNOWN 应返回 nil，实际 %v", ip)
	}
	rest, _ := br.Peek(4)
	if string(rest) != "next" {
		t.Fatalf("UNKNOWN 头行之后数据应保留，实际 %q", rest)
	}
}
