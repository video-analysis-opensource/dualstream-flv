package rtmp

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestEncodeMessage 校验消息分块封装与 Python 版本 encode_message 的结果一致。
func TestEncodeMessage(t *testing.T) {
	// 一条普通的 AMF0 命令消息（长度 191，单个 chunk 装得下）
	payload := bytes.Repeat([]byte{0xAB}, 191)

	got := EncodeMessage(0, 20, 1, payload)
	wantHeader := "030000000000bf1401000000"

	if hex.EncodeToString(got[:12]) != wantHeader {
		t.Errorf("消息头不一致\n got=%s\nwant=%s", hex.EncodeToString(got[:12]), wantHeader)
	}
	if len(got) != 12+len(payload) {
		t.Errorf("消息总长度期望 %d, got=%d", 12+len(payload), len(got))
	}
}

// TestEncodeMessageExtendedTimestamp 校验扩展时间戳与多 chunk 分片。
//
// 期望值来自原始 Python 实现：payload 6400 字节、时间戳 0xffffff+5 时，
// 整条消息长度为 6421 字节，消息头以 03ffffff001900090100000001000004 开头。
func TestEncodeMessageExtendedTimestamp(t *testing.T) {
	payload := bytes.Repeat(byteRange(64), 100) // 6400 字节

	got := EncodeMessage(0xffffff+5, 9, 1, payload)
	if len(got) != 6421 {
		t.Errorf("消息总长度期望 6421, got=%d", len(got))
	}

	wantHeader := "03ffffff00190009010000000100000400010203"
	if hex.EncodeToString(got[:20]) != wantHeader {
		t.Errorf("消息头不一致\n got=%s\nwant=%s", hex.EncodeToString(got[:20]), wantHeader)
	}

	// 第二个分片以 0xC3 开头，后跟 4 字节扩展时间戳
	splitPos := 16 + 4096
	if got[splitPos] != 0xC3 {
		t.Errorf("分片标记期望 0xC3, got=0x%02x", got[splitPos])
	}
	if hex.EncodeToString(got[splitPos+1:splitPos+5]) != "01000004" {
		t.Errorf("分片扩展时间戳不正确: %s",
			hex.EncodeToString(got[splitPos+1:splitPos+5]))
	}
}

// TestChunkReaderRoundTrip 校验「封装后再解析」可以还原出原始消息，
// 同时覆盖单 chunk 与多 chunk 两种情况。
func TestChunkReaderRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		timestamp uint32
		typeID    uint8
		payload   []byte
	}{
		{"单分片", 100, 20, bytes.Repeat([]byte{0x01}, 64)},
		{"多分片", 200, 9, bytes.Repeat(byteRange(64), 100)},
		{"扩展时间戳", 0xffffff + 5, 9, bytes.Repeat([]byte{0x02}, 10)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := EncodeMessage(tc.timestamp, tc.typeID, 1, tc.payload)

			reader := NewChunkReader(bytes.NewReader(encoded))
			// 发送端固定使用 4096 的 chunk 大小，接收端需保持一致
			reader.SetChunkSize(4096)

			msg, err := reader.ReadMessage()
			if err != nil {
				t.Fatalf("ReadMessage 返回错误: %v", err)
			}

			if msg.TypeID != tc.typeID {
				t.Errorf("消息类型期望 %d, got=%d", tc.typeID, msg.TypeID)
			}
			if msg.Timestamp != tc.timestamp {
				t.Errorf("时间戳期望 %d, got=%d", tc.timestamp, msg.Timestamp)
			}
			if !bytes.Equal(msg.Payload, tc.payload) {
				t.Errorf("负载内容不一致: 长度 %d vs %d", len(msg.Payload), len(tc.payload))
			}
		})
	}
}

// TestU24P24 校验 24 位整数的打包与解包互为逆运算。
func TestU24P24(t *testing.T) {
	for _, v := range []uint32{0, 1, 0x0102ab, 0xffffff} {
		if got := U24(P24(v)); got != v {
			t.Errorf("U24(P24(%d)) 期望 %d, got=%d", v, v, got)
		}
	}
}

// byteRange 返回 0..n-1 组成的字节切片，用于构造测试负载。
func byteRange(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}
