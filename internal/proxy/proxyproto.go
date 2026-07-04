package proxy

import (
	"bufio"
	"bytes"
	"net"
	"strings"
)

// proxyV1Prefix 是 Proxy Protocol V1 头的固定起始标记。
var proxyV1Prefix = []byte("PROXY ")

// readProxyProtocolV1 检测并解析连接首部的 Proxy Protocol V1 头。
//
// 若首部以 "PROXY " 开头，则消费该行并返回其中的真实源 IP；否则不消费任何字节
// （握手数据原样保留），返回 nil。仅解析 TCP4/TCP6，其余情形（UNKNOWN 或格式
// 异常）消费掉该行但返回 nil，交由调用方回落使用 socket 远端地址。
func readProxyProtocolV1(br *bufio.Reader) net.IP {
	// Peek 不消费缓冲，先确认是否为 PROXY 头，避免误吃掉正常的 MC 握手。
	head, err := br.Peek(len(proxyV1Prefix))
	if err != nil || !bytes.Equal(head, proxyV1Prefix) {
		return nil
	}

	// 确为 PROXY 头，读取整行（以 \n 结尾，V1 规范首行以 \r\n 终止）。
	line, err := br.ReadString('\n')
	if err != nil {
		return nil
	}
	line = strings.TrimRight(line, "\r\n")

	// 形如：PROXY TCP4 <src> <dst> <sport> <dport>
	fields := strings.Split(line, " ")
	if len(fields) < 6 {
		return nil
	}
	switch fields[1] {
	case "TCP4", "TCP6":
		return net.ParseIP(fields[2])
	default:
		return nil
	}
}
