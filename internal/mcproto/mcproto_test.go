package mcproto

import (
	"bufio"
	"bytes"
	"testing"
)

// VarInt 编解码应互为逆运算，覆盖边界值。
func TestVarIntRoundTrip(t *testing.T) {
	cases := []int32{0, 1, 2, 127, 128, 255, 256, 25565, 2097151, 766, 767, 768, -1}
	for _, v := range cases {
		enc := AppendVarInt(nil, v)
		got, err := ReadVarInt(bufio.NewReader(bytes.NewReader(enc)))
		if err != nil {
			t.Fatalf("ReadVarInt(%d) 出错: %v", v, err)
		}
		if got != v {
			t.Fatalf("VarInt 往返不符: 期望 %d 实际 %d", v, got)
		}
	}
}

// Login Success 在 766~767 应追加 Strict Error Handling 字节，768+ 不应追加。
// 这是版本碎片化最易出错处，必须锁死。
func TestLoginSuccessStrictErrorHandling(t *testing.T) {
	var uuid [16]byte
	name := "Steve"

	// 负载基础长度：16(UUID) + (1 长度前缀 + 5 名字) + 1(属性数=0) = 23。
	base := 16 + 1 + len(name) + 1

	withByte := BuildLoginSuccess(767, uuid, name) // 1.21.1
	if len(withByte) != base+1 {
		t.Fatalf("协议 767 应含 Strict Error Handling 字节，长度期望 %d 实际 %d", base+1, len(withByte))
	}
	if withByte[len(withByte)-1] != 0x00 {
		t.Fatalf("Strict Error Handling 应为 0x00，实际 0x%02X", withByte[len(withByte)-1])
	}

	withoutByte := BuildLoginSuccess(768, uuid, name) // 1.21.2
	if len(withoutByte) != base {
		t.Fatalf("协议 768 不应含 Strict Error Handling 字节，长度期望 %d 实际 %d", base, len(withoutByte))
	}
}

// 握手包应能被正确解析，且 Raw 保留可重放的原始帧。
func TestReadHandshake(t *testing.T) {
	var buf bytes.Buffer
	var data []byte
	data = AppendVarInt(data, 767)
	data = AppendString(data, "play.example.org")
	data = append(data, 0x63, 0xDD) // 端口 25565 = 0x63DD
	data = AppendVarInt(data, StateLogin)
	if err := WritePacket(&buf, 0x00, data); err != nil {
		t.Fatal(err)
	}

	raw := append([]byte(nil), buf.Bytes()...)
	hs, err := ReadHandshake(bufio.NewReader(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if hs.ProtocolVersion != 767 {
		t.Fatalf("协议版本期望 767 实际 %d", hs.ProtocolVersion)
	}
	if hs.ServerAddress != "play.example.org" {
		t.Fatalf("地址期望 play.example.org 实际 %q", hs.ServerAddress)
	}
	if hs.ServerPort != 25565 {
		t.Fatalf("端口期望 25565 实际 %d", hs.ServerPort)
	}
	if hs.NextState != StateLogin {
		t.Fatalf("NextState 期望 %d 实际 %d", StateLogin, hs.NextState)
	}
	if !bytes.Equal(hs.Raw, raw) {
		t.Fatalf("Raw 未能保留原始帧: 期望 %x 实际 %x", raw, hs.Raw)
	}
}
