// Package rtmp 实现 RTMP 协议的底层部分：握手、Chunk 分块读取、消息封装。
//
// 对应 Python 版本的 rtmp_server/rtmp.py。
package rtmp

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	// Version 是本服务支持的 RTMP 版本号（明文 RTMP）。
	Version = 3
	// HandshakeSize 是 C1/S1/C2/S2 各自的固定长度。
	HandshakeSize = 1536
	// DefaultChunkSize 是 RTMP 协议规定的初始 chunk 大小。
	DefaultChunkSize = 128
	// defaultOutChunkSize 是发送消息时使用的 chunk 大小，与 Python 版本保持一致。
	defaultOutChunkSize = 4096
	// extendedTimestamp 是「需要读取扩展时间戳」的哨兵值。
	extendedTimestamp = 0xffffff
)

// ReadExact 从 r 中精确读取 n 字节；对端关闭时返回错误。
func ReadExact(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("rtmp: 读取 %d 字节失败: %w", n, err)
	}
	return buf, nil
}

// U24 把 3 字节大端整数解析为 uint32。
func U24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

// P24 把整数打包成 3 字节大端序列。
func P24(v uint32) []byte {
	return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

// ServerHandshake 完成服务端侧的简单 RTMP 握手（C0/C1 -> S0/S1/S2 -> C2）。
//
// 与 Python 版本完全一致：S1 的前 4 字节为当前时间戳、随后 4 字节为 0、
// 其余为随机数；S2 直接回显客户端的 C1。OBS 等推流端接受这种标准握手。
func ServerHandshake(r io.Reader, w io.Writer) error {
	c0, err := ReadExact(r, 1)
	if err != nil {
		return err
	}
	if c0[0] != Version {
		return fmt.Errorf("rtmp: 不支持的 RTMP 版本 %d", c0[0])
	}

	c1, err := ReadExact(r, HandshakeSize)
	if err != nil {
		return err
	}

	s1 := make([]byte, HandshakeSize)
	binary.BigEndian.PutUint32(s1[0:4], uint32(time.Now().Unix()))
	binary.BigEndian.PutUint32(s1[4:8], 0)
	if _, err := rand.Read(s1[8:]); err != nil {
		return fmt.Errorf("rtmp: 生成握手随机数失败: %w", err)
	}

	// S0 + S1 + S2（S2 直接回显 C1）
	response := append(append([]byte{Version}, s1...), c1...)
	if _, err := w.Write(response); err != nil {
		return fmt.Errorf("rtmp: 发送握手响应失败: %w", err)
	}

	// 读取并丢弃 C2
	if _, err := ReadExact(r, HandshakeSize); err != nil {
		return err
	}
	return nil
}

// chunkState 保存单个 chunk stream（csid）上正在组装的消息状态。
type chunkState struct {
	timestamp uint32
	delta     uint32
	length    int
	typeID    uint8
	streamID  uint32
	received  int
	payload   []byte
}

// reset 清空正在组装的负载，为下一条消息做准备。
func (s *chunkState) reset() {
	s.received = 0
	s.payload = make([]byte, 0, s.length)
}

// Message 是一条组装完成的 RTMP 消息。
type Message struct {
	Timestamp uint32 // 消息时间戳（已处理扩展时间戳与 delta 累加）
	Length    int    // 消息长度
	TypeID    uint8  // 消息类型：1=SetChunkSize 4=UserControl 8=Audio 9=Video 18=Metadata 20=AMF0Command
	StreamID  uint32 // 消息流 ID
	Payload   []byte // 消息负载
}

// ChunkReader 按 RTMP Chunk 协议从连接中读取并组装完整消息。
//
// 注意：ChunkReader 不是并发安全的，一个连接对应一个 ChunkReader，
// 且只应由该连接自己的 goroutine 使用。
type ChunkReader struct {
	reader    io.Reader
	chunkSize int
	states    map[uint32]*chunkState
}

// NewChunkReader 创建 ChunkReader，初始 chunk 大小为协议默认的 128 字节。
func NewChunkReader(r io.Reader) *ChunkReader {
	return &ChunkReader{
		reader:    r,
		chunkSize: DefaultChunkSize,
		states:    make(map[uint32]*chunkState),
	}
}

// SetChunkSize 更新接收端 chunk 大小（收到 SetChunkSize 消息时调用）。
func (cr *ChunkReader) SetChunkSize(size int) {
	cr.chunkSize = size
}

// basicHeader 读取 Basic Header，返回 (fmt, csid)。
func (cr *ChunkReader) basicHeader() (uint8, uint32, error) {
	head, err := ReadExact(cr.reader, 1)
	if err != nil {
		return 0, 0, err
	}

	format := head[0] >> 6
	csid := uint32(head[0] & 0x3f)

	switch csid {
	case 0: // 2 字节形式
		ext, err := ReadExact(cr.reader, 1)
		if err != nil {
			return 0, 0, err
		}
		csid = uint32(ext[0]) + 64
	case 1: // 3 字节形式
		ext, err := ReadExact(cr.reader, 2)
		if err != nil {
			return 0, 0, err
		}
		csid = uint32(ext[0]) + uint32(ext[1])<<8 + 64
	}
	return format, csid, nil
}

// readExtendedTimestamp 在时间戳字段为 0xffffff 时读取 4 字节扩展时间戳。
func (cr *ChunkReader) readExtendedTimestamp() (uint32, error) {
	raw, err := ReadExact(cr.reader, 4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(raw), nil
}

// state 返回 csid 对应的组装状态，不存在则创建。
func (cr *ChunkReader) state(csid uint32) *chunkState {
	if st, ok := cr.states[csid]; ok {
		return st
	}
	st := &chunkState{}
	cr.states[csid] = st
	return st
}

// ReadMessage 持续读取 chunk，直到组装出一条完整消息为止。
func (cr *ChunkReader) ReadMessage() (Message, error) {
	for {
		format, csid, err := cr.basicHeader()
		if err != nil {
			return Message{}, err
		}
		st := cr.state(csid)

		if err := cr.readMessageHeader(format, st); err != nil {
			return Message{}, err
		}

		// 本次 chunk 能携带的负载：min(chunk 大小, 消息剩余长度)
		remaining := st.length - st.received
		if n := min(cr.chunkSize, remaining); n > 0 {
			body, err := ReadExact(cr.reader, n)
			if err != nil {
				return Message{}, err
			}
			st.payload = append(st.payload, body...)
			st.received += n
		}

		if st.received == st.length {
			msg := Message{
				Timestamp: st.timestamp,
				Length:    st.length,
				TypeID:    st.typeID,
				StreamID:  st.streamID,
				Payload:   st.payload,
			}
			st.reset()
			return msg, nil
		}
	}
}

// readMessageHeader 按 fmt 类型读取 Message Header 并更新 chunk 状态。
//
//	fmt=0：11 字节完整头（绝对时间戳）
//	fmt=1：7 字节（时间戳增量 + 长度 + 类型）
//	fmt=2：3 字节（仅时间戳增量）
//	fmt=3：无头，沿用上一条消息的信息
func (cr *ChunkReader) readMessageHeader(format uint8, st *chunkState) error {
	switch format {
	case 0:
		h, err := ReadExact(cr.reader, 11)
		if err != nil {
			return err
		}
		ts := U24(h[0:3])
		st.length = int(U24(h[3:6]))
		st.typeID = h[6]
		st.streamID = binary.LittleEndian.Uint32(h[7:11]) // 消息流 ID 是小端
		st.timestamp = ts
		st.delta = 0
		st.reset()

		if ts == extendedTimestamp {
			if st.timestamp, err = cr.readExtendedTimestamp(); err != nil {
				return err
			}
		}

	case 1:
		h, err := ReadExact(cr.reader, 7)
		if err != nil {
			return err
		}
		delta := U24(h[0:3])
		st.length = int(U24(h[3:6]))
		st.typeID = h[6]
		st.delta = delta
		st.reset()

		if delta == extendedTimestamp {
			if st.delta, err = cr.readExtendedTimestamp(); err != nil {
				return err
			}
		}
		st.timestamp += st.delta

	case 2:
		h, err := ReadExact(cr.reader, 3)
		if err != nil {
			return err
		}
		delta := U24(h)
		st.delta = delta
		st.reset()

		if delta == extendedTimestamp {
			if st.delta, err = cr.readExtendedTimestamp(); err != nil {
				return err
			}
		}
		st.timestamp += st.delta

	case 3:
		if st.length <= 0 {
			return fmt.Errorf("rtmp: fmt=3 状态非法（尚未收到过完整消息头）")
		}
		// 只有在开始组装新消息时才累加时间戳增量；
		// 消息被拆成多个 chunk 时的后续 chunk 不能重复累加。
		if st.received == 0 {
			st.timestamp += st.delta
		}

	default:
		return fmt.Errorf("rtmp: 非法的 chunk format %d", format)
	}
	return nil
}

// EncodeMessage 把一条消息封装成 chunk 序列（发送方向）。
//
// 首个 chunk 使用 fmt=0 / csid=3 的完整头，后续分片使用 0xC3（fmt=3 / csid=3）。
func EncodeMessage(timestamp uint32, typeID uint8, streamID uint32, payload []byte) []byte {
	return encodeMessageWithChunkSize(timestamp, typeID, streamID, payload, defaultOutChunkSize)
}

// encodeMessageWithChunkSize 是 EncodeMessage 的可指定 chunk 大小版本。
func encodeMessageWithChunkSize(timestamp uint32, typeID uint8, streamID uint32, payload []byte, chunkSize int) []byte {
	length := len(payload)
	ts24 := min(timestamp, extendedTimestamp)

	out := make([]byte, 0, length+16)
	out = append(out, 0x03) // fmt=0, csid=3
	out = append(out, P24(ts24)...)
	out = append(out, P24(uint32(length))...)
	out = append(out, typeID)

	streamIDBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(streamIDBytes, streamID) // 消息流 ID 是小端
	out = append(out, streamIDBytes...)

	if timestamp >= extendedTimestamp {
		out = append(out, be32(timestamp)...)
	}

	for pos := 0; pos < length; {
		n := min(chunkSize, length-pos)
		if pos > 0 {
			out = append(out, 0xC3) // fmt=3, csid=3，表示同一消息的后续分片
			if timestamp >= extendedTimestamp {
				out = append(out, be32(timestamp)...)
			}
		}
		out = append(out, payload[pos:pos+n]...)
		pos += n
	}
	return out
}

// Ack 生成 Acknowledgement 消息的负载（4 字节大端序列号）。
func Ack(seq uint32) []byte {
	return be32(seq)
}

// be32 返回大端 uint32 字节序列。
func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
