package server

import (
	"testing"

	"dualstream-flv/internal/session"
)

// TestBufferGOPAndExpire 校验 GOP 长度计算与旧帧淘汰逻辑。
//
// 场景：每 10 帧一个关键帧（第 1、11、21... 帧），共写入 60 帧。
// 所有期望值均来自原始 Python 实现 dualstream-flv.py 中的 RTMPBuffer，
// 用于确保 Go 版本的缓冲行为与 Python 版本完全一致。
func TestBufferGOPAndExpire(t *testing.T) {
	// 每个检查点的期望状态
	expectations := map[int]struct {
		gopLength          int
		maxLength          int
		lengthCount        int
		lastExpiredKey     int
		lastKeyframeCursor int
	}{
		1:  {0, 0, 1, 0, 1},
		10: {0, 0, 10, 0, 1},
		11: {10, 30, 11, 0, 11},
		12: {10, 30, 12, 0, 11},
		30: {10, 30, 30, 0, 21},
		31: {10, 30, 30, 1, 31},
		32: {10, 30, 30, 2, 31},
		41: {10, 30, 30, 11, 41},
		60: {10, 30, 30, 30, 51},
	}

	buf := NewBuffer()
	meta := Meta{SPS: []byte{0x67}, PPS: []byte{0x68}, Width: 1920, Height: 1080}

	for i := 1; i <= 60; i++ {
		buf.Append(session.VideoFrame{
			StreamKey:  "chan0",
			PTS:        int64(i * 40),
			IsKeyframe: i%10 == 1,
		}, meta)

		want, checked := expectations[i]
		if !checked {
			continue
		}

		if buf.gopLength != want.gopLength || buf.maxLength != want.maxLength {
			t.Errorf("第 %d 帧: GOP/最大长度期望 %d/%d, got %d/%d",
				i, want.gopLength, want.maxLength, buf.gopLength, buf.maxLength)
		}
		if buf.lengthCount != want.lengthCount || buf.lastExpiredKey != want.lastExpiredKey {
			t.Errorf("第 %d 帧: 长度/淘汰键期望 %d/%d, got %d/%d",
				i, want.lengthCount, want.lastExpiredKey, buf.lengthCount, buf.lastExpiredKey)
		}
		if buf.lastKeyframeCursor != want.lastKeyframeCursor {
			t.Errorf("第 %d 帧: 最近关键帧键期望 %d, got %d",
				i, want.lastKeyframeCursor, buf.lastKeyframeCursor)
		}

		// 已被淘汰的帧必须取不到，最旧的一帧必须还在
		if want.lastExpiredKey > 0 {
			if _, ok := buf.Get(want.lastExpiredKey); ok {
				t.Errorf("第 %d 帧: 键 %d 应该已被淘汰", i, want.lastExpiredKey)
			}
		}
		if _, err := buf.EarlyItem(); err != nil {
			t.Errorf("第 %d 帧: EarlyItem 返回错误: %v", i, err)
		}
	}

	// 最新一帧与元数据
	last, err := buf.LastItem()
	if err != nil {
		t.Fatalf("LastItem 返回错误: %v", err)
	}
	if last.PTS != 60*40 {
		t.Errorf("最新一帧 PTS 期望 %d, got %d", 60*40, last.PTS)
	}
	if got := buf.Meta(); got.Width != 1920 || got.Height != 1080 {
		t.Errorf("元数据分辨率期望 1920x1080, got %dx%d", got.Width, got.Height)
	}
}

// TestEmptyBuffer 校验空缓冲区的取值行为（对应 Python 版本抛出 "empty buffer!!"）。
func TestEmptyBuffer(t *testing.T) {
	buf := NewBuffer()

	if _, err := buf.LastItem(); err == nil {
		t.Error("空缓冲区 LastItem 期望返回错误")
	}
	if _, err := buf.LastKey(); err == nil {
		t.Error("空缓冲区 LastKey 期望返回错误")
	}
	if _, err := buf.EarlyItem(); err == nil {
		t.Error("空缓冲区 EarlyItem 期望返回错误")
	}
}

// TestBufferKeys 校验预置的流名列表：chan0 ~ chan10 以及各自的辅助流。
func TestBufferKeys(t *testing.T) {
	keys := BufferKeys()
	if len(keys) != 22 {
		t.Fatalf("期望 22 个流名, got=%d", len(keys))
	}

	server := NewRTMPServer("127.0.0.1", 0, 8)
	for _, key := range []string{"chan0", "chan0-auxiliary", "chan10", "chan10-auxiliary"} {
		if server.Buffer(key) == nil {
			t.Errorf("流名 %s 应该已预置缓冲区", key)
		}
	}
	if server.Buffer("chan11") != nil {
		t.Error("流名 chan11 不应该存在")
	}
}
