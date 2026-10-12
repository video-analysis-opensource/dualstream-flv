// Package server 实现 RTMP 接收服务与 HTTP-FLV 分发服务。
//
// buffer.go 对应 Python 版本 dualstream-flv.py 中的 RTMPBuffer：
// 一个按写入顺序编号、自动淘汰旧数据的环形帧缓冲区。
package server

import (
	"fmt"
	"sync"

	"dualstream-flv/internal/session"
)

// Meta 是一路流的解码参数，随每一帧一起更新。
type Meta struct {
	PPS           []byte // 图像参数集
	SPS           []byte // 序列参数集
	Width         int    // 视频宽度
	Height        int    // 视频高度
	NALLengthSize int    // AVCC 帧里 NALU 长度字段的字节数
}

// Buffer 是单路流的视频帧缓冲区。
//
// 每写入一帧，游标 cursor 自增 1 并作为该帧的键；
// 当缓冲区长度超过 3 个 GOP 时，从最旧的一帧开始淘汰。
//
// Buffer 会被 RTMP 接收 goroutine 与多个 HTTP 播放 goroutine 并发访问，
// 因此所有对外方法都在读写锁保护下工作。
type Buffer struct {
	mu     sync.RWMutex
	frames map[int]session.VideoFrame

	meta Meta // 当前流的解码参数

	lengthCount        int // 当前缓存的帧数
	lastExpiredKey     int // 最近一次被淘汰的帧的键
	lastKeyframeCursor int // 最近一个关键帧所在的游标
	cursor             int // 最新一帧的键（也是累计写入帧数）
	maxLength          int // 缓冲区最大长度（3 倍 GOP）
	gopLength          int // 一个 GOP 内的帧数量
}

// NewBuffer 创建一个空的帧缓冲区。
//
// NALLengthSize 先给一个 4 的默认值（与 Python 版本 RTMPBuffer 一致），
// 收到第一帧后会被推流实际声明的值覆盖。
func NewBuffer() *Buffer {
	return &Buffer{
		frames: make(map[int]session.VideoFrame),
		meta:   Meta{NALLengthSize: 4},
	}
}

// Append 写入一帧，同时更新该路流的解码参数。
//
// 解码参数与帧在同一把锁内更新，保证播放端读到的 SPS/PPS 与帧数据一致。
func (b *Buffer) Append(frame session.VideoFrame, meta Meta) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.meta = meta

	b.lengthCount++
	b.cursor++
	b.frames[b.cursor] = frame

	if frame.IsKeyframe {
		// 用两个关键帧之间的间隔计算一个 GOP 内的帧数量
		if b.lastKeyframeCursor != 0 && b.lastKeyframeCursor != b.cursor {
			b.gopLength = b.cursor - b.lastKeyframeCursor
		}
		// 最大长度是 3 倍的 GOP 长度（GOP 长度未知前保持 0，即不淘汰）
		if b.maxLength == 0 {
			b.maxLength = b.gopLength * 3
		}
		b.lastKeyframeCursor = b.cursor
	}

	// 超出最大长度时淘汰最旧的一帧
	if b.maxLength != 0 && b.lengthCount > b.maxLength {
		oldestKey := b.lastExpiredKey + 1
		delete(b.frames, oldestKey)
		b.lastExpiredKey = oldestKey
		b.lengthCount--
	}
}

// Get 按键取出一帧；帧已被淘汰或尚未写入时返回 false。
func (b *Buffer) Get(key int) (session.VideoFrame, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	frame, ok := b.frames[key]
	return frame, ok
}

// Meta 返回当前的解码参数快照。
func (b *Buffer) Meta() Meta {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.meta
}

// GOPLength 返回一个 GOP 内的帧数量。
func (b *Buffer) GOPLength() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.gopLength
}

// LastKeyframeCursor 返回最近一个关键帧所在的键。
func (b *Buffer) LastKeyframeCursor() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.lastKeyframeCursor
}

// LastItem 返回最新写入的一帧；缓冲区为空时返回错误。
func (b *Buffer) LastItem() (session.VideoFrame, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.cursor == 0 {
		return session.VideoFrame{}, fmt.Errorf("empty buffer!! %s", b.describe())
	}
	return b.frames[b.cursor], nil
}

// LastKey 返回最新写入的一帧的键；缓冲区为空时返回错误。
func (b *Buffer) LastKey() (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.cursor == 0 {
		return 0, fmt.Errorf("empty buffer!! %s", b.describe())
	}
	return b.cursor, nil
}

// EarlyItem 返回当前缓冲区中最旧的一帧；缓冲区为空时返回错误。
func (b *Buffer) EarlyItem() (session.VideoFrame, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	frame, ok := b.frames[b.lastExpiredKey+1]
	if !ok {
		return session.VideoFrame{}, fmt.Errorf("empty buffer!! %s", b.describe())
	}
	return frame, nil
}

// String 输出缓冲区状态，格式与 Python 版本保持一致，便于对照日志。
func (b *Buffer) String() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.describe()
}

// describe 是 String 的内部实现，调用方需自行持有锁。
func (b *Buffer) describe() string {
	return fmt.Sprintf("<RTMPBuffer-%p-GOPLen-%d-MAX(%d)-Length%d-LastExpiredKey%d>",
		b, b.gopLength, b.maxLength, b.lengthCount, b.lastExpiredKey)
}
