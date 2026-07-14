// Package mcproto 实现 NETTOFRP 需要的最小 Minecraft Java 版协议子集：
// 读取握手/登录包、构造 Login Success 与 Transfer 包。
//
// 只涉及协议中多年稳定的少数结构（握手、登录起始、登录成功、Transfer），
// 不触碰任何游戏内封包，因此对客户端版本升级基本免疫。
package mcproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// 连接状态（握手包的 Next State 字段）。
const (
	StateStatus = 1 // 服务器列表 ping
	StateLogin  = 2 // 登录
)

// TransferMinProtocol 是支持 Transfer 包的最低协议版本。
// Transfer（configuration 状态）在 1.20.5（协议 766）引入。
const TransferMinProtocol = 766

// strictErrorHandlingMaxProtocol 是 Login Success 仍带 Strict Error Handling
// 布尔字段的最高协议版本。766(1.20.5)~767(1.21.1) 需要该字段，768(1.21.2) 起移除。
const strictErrorHandlingMaxProtocol = 767

var errVarIntTooBig = errors.New("VarInt 超过 5 字节")

// ReadVarInt 从 r 读取一个 Minecraft VarInt。
func ReadVarInt(r io.ByteReader) (int32, error) {
	var value uint32
	var position uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		value |= uint32(b&0x7F) << position
		if b&0x80 == 0 {
			break
		}
		position += 7
		if position >= 32 {
			return 0, errVarIntTooBig
		}
	}
	return int32(value), nil
}

// AppendVarInt 将 v 以 VarInt 编码追加到 dst。
func AppendVarInt(dst []byte, v int32) []byte {
	uv := uint32(v)
	for {
		b := byte(uv & 0x7F)
		uv >>= 7
		if uv != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if uv == 0 {
			return dst
		}
	}
}

// AppendString 将带 VarInt 长度前缀的 UTF-8 字符串追加到 dst。
func AppendString(dst []byte, s string) []byte {
	dst = AppendVarInt(dst, int32(len(s)))
	return append(dst, s...)
}

// byteReader 同时支持逐字节与批量读取，bytes.Reader 与 bufio.Reader 均满足。
type byteReader interface {
	io.Reader
	io.ByteReader
}

// readString 读取带 VarInt 长度前缀的字符串。
func readString(r byteReader) (string, error) {
	n, err := ReadVarInt(r)
	if err != nil {
		return "", err
	}
	if n < 0 || n > 32767 {
		return "", fmt.Errorf("字符串长度非法: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Packet 是一个未压缩的数据包：ID + 负载。
type Packet struct {
	ID   int32
	Data []byte // 不含长度前缀与包 ID
}

// maxHandshakeBytes 是握手/登录包允许的最大帧长度。
// 握手包：协议版本(5) + 地址(255+2) + 端口(2) + 状态(1) ≈ 270 字节，
// 留出足够余量但远小于通用上限 2 MiB，防止未认证连接大量分配内存。
const maxHandshakeBytes = 1024

// ReadPacket 读取一个未压缩数据包（长度前缀 + VarInt 包 ID + 负载）。
// 登录阶段在开启压缩前始终为未压缩格式，NETTOFRP 不会开启压缩。
func ReadPacket(r *bufio.Reader) (Packet, error) {
	return readPacket(r, 2097151)
}

// readHandshakePacket 与 ReadPacket 相同，但对帧长度施加更严格的上限，
// 防止未认证的握手阶段通过声明大长度来大量分配内存（内存 DoS）。
func readHandshakePacket(r *bufio.Reader) (Packet, error) {
	return readPacket(r, maxHandshakeBytes)
}

func readPacket(r *bufio.Reader, maxLen int32) (Packet, error) {
	length, err := ReadVarInt(r)
	if err != nil {
		return Packet{}, err
	}
	if length <= 0 || length > maxLen {
		return Packet{}, fmt.Errorf("包长度非法: %d", length)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(r, frame); err != nil {
		return Packet{}, err
	}

	br := bytes.NewReader(frame)
	id, err := ReadVarInt(br)
	if err != nil {
		return Packet{}, err
	}
	data := make([]byte, br.Len())
	if _, err := io.ReadFull(br, data); err != nil {
		return Packet{}, err
	}
	return Packet{ID: id, Data: data}, nil
}

// WritePacket 将 id+data 以未压缩格式（含长度前缀）写入 w。
func WritePacket(w io.Writer, id int32, data []byte) error {
	var body []byte
	body = AppendVarInt(body, id)
	body = append(body, data...)

	var out []byte
	out = AppendVarInt(out, int32(len(body)))
	out = append(out, body...)

	_, err := w.Write(out)
	return err
}

// Handshake 是握手包的解析结果。
type Handshake struct {
	ProtocolVersion int32
	ServerAddress   string
	ServerPort      uint16
	NextState       int32
	Raw             []byte // 原始完整帧，供回落时向上游重放
}

// ReadHandshake 读取并解析握手包（包 ID 0x00）。
// 同时保留原始字节，便于回落到纯 TCP 转发时向上游重放。
func ReadHandshake(r *bufio.Reader) (Handshake, error) {
	pkt, err := readHandshakePacket(r)
	if err != nil {
		return Handshake{}, err
	}
	if pkt.ID != 0x00 {
		return Handshake{}, fmt.Errorf("期望握手包(0x00)，实际 0x%02X", pkt.ID)
	}

	br := bytes.NewReader(pkt.Data)
	proto, err := ReadVarInt(br)
	if err != nil {
		return Handshake{}, err
	}
	addr, err := readString(br)
	if err != nil {
		return Handshake{}, err
	}
	portHi, err := br.ReadByte()
	if err != nil {
		return Handshake{}, err
	}
	portLo, err := br.ReadByte()
	if err != nil {
		return Handshake{}, err
	}
	next, err := ReadVarInt(br)
	if err != nil {
		return Handshake{}, err
	}

	return Handshake{
		ProtocolVersion: proto,
		ServerAddress:   addr,
		ServerPort:      uint16(portHi)<<8 | uint16(portLo),
		NextState:       next,
		Raw:             reencodePacket(pkt),
	}, nil
}

// LoginStart 是登录起始包的解析结果。
type LoginStart struct {
	Name string
	UUID [16]byte
}

// ReadLoginStart 读取并解析登录起始包（包 ID 0x00，Login 状态）。
// 协议 ≥759(1.19) 起 UUID 字段存在；本函数仅用于 ≥766 的 Transfer 路径，UUID 恒存在。
func ReadLoginStart(r *bufio.Reader) (LoginStart, error) {
	pkt, err := readHandshakePacket(r)
	if err != nil {
		return LoginStart{}, err
	}
	if pkt.ID != 0x00 {
		return LoginStart{}, fmt.Errorf("期望登录起始包(0x00)，实际 0x%02X", pkt.ID)
	}

	br := bytes.NewReader(pkt.Data)
	name, err := readString(br)
	if err != nil {
		return LoginStart{}, err
	}
	var ls LoginStart
	ls.Name = name
	if _, err := io.ReadFull(br, ls.UUID[:]); err != nil {
		// 少数客户端可能不带 UUID，容忍之（用零值）。
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ls, nil
		}
		return LoginStart{}, err
	}
	return ls, nil
}

// BuildLoginSuccess 构造 Login Success 包（Login 状态，包 ID 0x02）的负载。
// 依据协议版本决定是否追加 Strict Error Handling 布尔字段。
func BuildLoginSuccess(proto int32, uuid [16]byte, name string) []byte {
	var data []byte
	data = append(data, uuid[:]...)
	data = AppendString(data, name)
	data = AppendVarInt(data, 0) // 属性数量 = 0
	if proto >= TransferMinProtocol && proto <= strictErrorHandlingMaxProtocol {
		data = append(data, 0x00) // Strict Error Handling = false
	}
	return data
}

// BuildTransfer 构造 Transfer 包（configuration 状态）的负载：host + port。
func BuildTransfer(host string, port uint16) []byte {
	var data []byte
	data = AppendString(data, host)
	data = AppendVarInt(data, int32(port))
	return data
}

// reencodePacket 把已解析的 Packet 重新编码为完整帧（长度前缀 + ID + 负载）。
func reencodePacket(p Packet) []byte {
	var body []byte
	body = AppendVarInt(body, p.ID)
	body = append(body, p.Data...)
	var out []byte
	out = AppendVarInt(out, int32(len(body)))
	return append(out, body...)
}
