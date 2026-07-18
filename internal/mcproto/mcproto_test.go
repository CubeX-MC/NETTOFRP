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
func TestLoginSuccessStrictErrorHandling(t *testing.T) {
	var uuid [16]byte
	name := "Steve"
	base := 16 + 1 + len(name) + 1

	withByte := BuildLoginSuccess(767, uuid, name, [16]byte{})
	if len(withByte) != base+1 {
		t.Fatalf("协议 767 应含 Strict Error Handling 字节，长度期望 %d 实际 %d", base+1, len(withByte))
	}
	if withByte[len(withByte)-1] != 0x00 {
		t.Fatalf("Strict Error Handling 应为 0x00，实际 0x%02X", withByte[len(withByte)-1])
	}

	withoutByte := BuildLoginSuccess(768, uuid, name, [16]byte{})
	if len(withoutByte) != base {
		t.Fatalf("协议 768 不应含 Strict Error Handling 字节，长度期望 %d 实际 %d", base, len(withoutByte))
	}
}

// Minecraft 26.2（协议 776）的 Login Finished 末尾必须带 Session UUID。
func TestLoginSuccess26_2IncludesSessionID(t *testing.T) {
	var profileID [16]byte
	sessionID := [16]byte{0, 1, 2, 3, 4, 5, 0x46, 7, 0x88, 9, 10, 11, 12, 13, 14, 15}
	name := "Steve"
	base := 16 + 1 + len(name) + 1

	data := BuildLoginSuccess(776, profileID, name, sessionID)
	if len(data) != base+len(sessionID) {
		t.Fatalf("协议 776 负载长度期望 %d 实际 %d", base+len(sessionID), len(data))
	}
	if !bytes.Equal(data[len(data)-len(sessionID):], sessionID[:]) {
		t.Fatalf("Login Finished 末尾未携带期望的 Session UUID")
	}
}

func TestSupportsTransferUsesVerifiedProtocolRange(t *testing.T) {
	for _, tc := range []struct {
		proto int32
		want  bool
	}{
		{765, false},
		{766, true},
		{776, true},
		{777, false},
	} {
		if got := SupportsTransfer(tc.proto); got != tc.want {
			t.Errorf("SupportsTransfer(%d) = %t，期望 %t", tc.proto, got, tc.want)
		}
	}
}

// 握手包应能被正确解析，且 Raw 保留可重放的原始帧。
func TestReadHandshake(t *testing.T) {
	var buf bytes.Buffer
	var data []byte
	data = AppendVarInt(data, 767)
	data = AppendString(data, "play.example.org")
	data = append(data, 0x63, 0xDD)
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

// 超过握手上限（1024字节）的帧应被拒绝，防止内存 DoS。
func TestReadHandshakeRejectsOversizedFrame(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(AppendVarInt(nil, maxHandshakeBytes+1))
	buf.Write(make([]byte, maxHandshakeBytes+1))

	_, err := ReadHandshake(bufio.NewReader(&buf))
	if err == nil {
		t.Fatal("期望拒绝超大握手帧，但无错误返回")
	}
}

// 通用 ReadPacket 仍允许大帧，不受握手限制影响。
func TestReadPacketAllowsLargeFrame(t *testing.T) {
	payload := make([]byte, 512)
	var buf bytes.Buffer
	if err := WritePacket(&buf, 0x00, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPacket(bufio.NewReader(&buf)); err != nil {
		t.Fatalf("ReadPacket 512 字节负载应通过，实际: %v", err)
	}
}
