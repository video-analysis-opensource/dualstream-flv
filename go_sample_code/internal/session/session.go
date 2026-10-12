// Package session 实现单条 RTMP 推流连接的会话处理。
//
// 对应 Python 版本的 rtmp_server/session.py：完成握手、处理 AMF0 命令
// （connect / createStream / publish 等），并把解析出的音视频帧通过回调交给上层。
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"

	"dualstream-flv/internal/amf0"
	"dualstream-flv/internal/flv"
	"dualstream-flv/internal/rtmp"
)

// RTMP 消息类型（Message Type ID）。
const (
	msgSetChunkSize = 1
	msgUserControl  = 4
	msgAudio        = 8
	msgVideo        = 9
	msgAMF3Command  = 17
	msgMetadata     = 18
	msgAMF0Command  = 20
)

// maxChunkSize 是允许对端设置的最大 chunk 大小（16MB），防止异常值撑爆内存。
const maxChunkSize = 16 * 1024 * 1024

// VideoFrame 是一帧视频数据。
//
// AVCCData 为 AVCC 格式（[长度][NALU]...）的裸帧数据，可直接写入 FLV Tag。
type VideoFrame struct {
	StreamKey  string // 推流的 stream key，例如 chan0 / chan0-auxiliary
	DTS        int64  // 解码时间戳
	PTS        int64  // 显示时间戳
	IsKeyframe bool   // 是否关键帧
	AVCCData   []byte // AVCC 格式帧数据

	// dual-stream 输出时，同一帧可能需要把 slice header 里的
	// pic_parameter_set_id 改写成 main/aux 各自的编号。
	// 一帧会被多个播放连接共用，这里缓存改写结果，key 是目标 pps_id。
	// 详见 flv.DualStreamParameterSets.Remap。
	//
	// VideoFrame 以值的形式在缓冲区里流转，这里是指针，所以所有副本
	// 共用同一份缓存。
	RemapCache *flv.FrameCache
}

// AudioFrame 是一帧音频数据。
type AudioFrame struct {
	StreamKey string // 推流的 stream key
	Timestamp int64  // 时间戳
	Data      []byte // 裸音频数据
}

// VideoHandler 是视频帧回调；上层通过它把帧写入缓冲区。
type VideoHandler func(frame VideoFrame, s *Session)

// AudioHandler 是音频帧回调。
type AudioHandler func(frame AudioFrame, s *Session)

// Session 表示一条 RTMP 推流会话。
//
// 一个 Session 只在自己的 goroutine 内被使用，回调也在该 goroutine 内同步执行，
// 因此内部字段无需加锁。
type Session struct {
	conn      net.Conn
	addr      string
	bufReader *bufio.Reader     // 带缓冲的读取器，握手与 chunk 读取共用同一个，避免数据被截断
	writer    *bufio.Writer     // 带缓冲的写入器
	reader    *rtmp.ChunkReader // chunk 组装器

	App         string // connect 命令中的 app 名
	TcURL       string // connect 命令中的 tcUrl
	StreamKey   string // publish 命令中的流名
	Publishing  bool   // 是否已经开始推流
	outStreamID uint32 // createStream 分配的流 ID

	AVCConfig     []byte // 原始 AVCDecoderConfigurationRecord
	SPS           []byte // 序列参数集
	PPS           []byte // 图像参数集
	NALLengthSize int    // NALU 长度字段字节数
	AACConfig     []byte // AAC AudioSpecificConfig
	Width         int    // 视频宽度（由 SPS 解析得到）
	Height        int    // 视频高度（由 SPS 解析得到）

	VideoCount int // 已接收视频帧数
	AudioCount int // 已接收音频帧数

	onVideo VideoHandler
	onAudio AudioHandler
}

// Option 是创建 Session 时的可选配置（函数式选项模式）。
type Option func(*Session)

// WithVideoHandler 设置视频帧回调。
func WithVideoHandler(h VideoHandler) Option {
	return func(s *Session) { s.onVideo = h }
}

// WithAudioHandler 设置音频帧回调。
func WithAudioHandler(h AudioHandler) Option {
	return func(s *Session) { s.onAudio = h }
}

// New 基于一条已建立的 TCP 连接创建 RTMP 会话。
func New(conn net.Conn, opts ...Option) *Session {
	reader := bufio.NewReaderSize(conn, 64*1024)

	s := &Session{
		conn:          conn,
		addr:          conn.RemoteAddr().String(),
		bufReader:     reader,
		writer:        bufio.NewWriterSize(conn, 64*1024),
		reader:        rtmp.NewChunkReader(reader),
		NALLengthSize: 4,
		outStreamID:   1,
	}

	// 依次应用所有可选配置
	for _, apply := range opts {
		apply(s)
	}
	return s
}

// Addr 返回对端地址，供日志使用。
func (s *Session) Addr() string { return s.addr }

// send 发送一条 RTMP 消息并立刻刷新缓冲区。
func (s *Session) send(timestamp uint32, typeID uint8, streamID uint32, payload []byte) error {
	if _, err := s.writer.Write(rtmp.EncodeMessage(timestamp, typeID, streamID, payload)); err != nil {
		return fmt.Errorf("session: 写入消息失败: %w", err)
	}
	return s.writer.Flush()
}

// command 发送一条 AMF0 命令消息。
func (s *Session) command(name string, tid float64, args ...any) error {
	payload, err := amf0.EncodeCommand(name, tid, args...)
	if err != nil {
		return err
	}
	return s.send(0, msgAMF0Command, s.outStreamID, payload)
}

// handleConnect 处理 connect 命令：回应窗口大小、带宽、chunk 大小，并返回 _result。
func (s *Session) handleConnect(tid float64, obj any) error {
	if props, ok := obj.(map[string]any); ok {
		s.App, _ = props["app"].(string)
		s.TcURL, _ = props["tcUrl"].(string)
	}

	// Window Acknowledgement Size
	if err := s.send(0, 5, 0, be32(5_000_000)); err != nil {
		return err
	}
	// Set Peer Bandwidth（0x02 = dynamic）
	if err := s.send(0, 6, 0, append(be32(5_000_000), 0x02)); err != nil {
		return err
	}
	// Set Chunk Size（发送方向使用 4096）
	if err := s.send(0, 1, 0, be32(4096)); err != nil {
		return err
	}

	return s.command("_result", tid,
		amf0.NewObject(
			"fmsVer", "FMS/3,5,7,7009",
			"capabilities", 31.0,
		),
		amf0.NewObject(
			"level", "status",
			"code", "NetConnection.Connect.Success",
			"description", "Connection succeeded.",
			"objectEncoding", 0.0,
		),
	)
}

// handleCommandMessage 解析并分发 AMF0 命令消息。
func (s *Session) handleCommandMessage(payload []byte) error {
	values, err := amf0.DecodeAll(payload)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}

	name, _ := values[0].(string)

	tid := 0.0
	if len(values) > 1 {
		tid, _ = values[1].(float64)
	}

	args := values[2:]

	switch name {
	case "connect":
		var obj any = map[string]any{}
		if len(args) > 0 {
			obj = args[0]
		}
		return s.handleConnect(tid, obj)

	case "createStream":
		s.outStreamID = 1
		return s.command("_result", tid, nil, float64(s.outStreamID))

	case "publish":
		s.StreamKey = "unknown"
		if len(args) > 1 {
			s.StreamKey = asString(args[1])
		}
		s.Publishing = true

		if err := s.command("onStatus", 0, nil, amf0.NewObject(
			"level", "status",
			"code", "NetStream.Publish.Start",
			"description", "Publishing "+s.StreamKey,
		)); err != nil {
			return err
		}
		log.Printf("[rtmp.session] publish %s from %s", s.StreamKey, s.addr)

	case "releaseStream", "FCPublish", "FCUnpublish":
		// 这些命令无需处理，忽略即可

	case "deleteStream":
		s.Publishing = false
	}
	return nil
}

// handleVideoMessage 处理视频消息：序列头用于提取 SPS/PPS/宽高，其余作为视频帧回调出去。
func (s *Session) handleVideoMessage(timestamp uint32, payload []byte) error {
	info, err := flv.ParseVideoTag(payload, timestamp)
	if err != nil {
		return err
	}
// 	log.Printf("###video_message### %d", info.PacketType)

	// packet_type == 0：AVC 序列头
	if info.PacketType == 0 {
		cfg, err := flv.ParseAVCConfig(info.Payload)
		if err != nil {
			return err
		}

		s.AVCConfig = info.Payload
		s.SPS = cfg.SPS
		s.PPS = cfg.PPS
		s.NALLengthSize = cfg.NALLengthSize
		s.Width = cfg.SPSInfo.Width
		s.Height = cfg.SPSInfo.Height

		log.Printf("[rtmp.session] stream=%s SPS=%d PPS=%d NALLength=%d profile=0x%02x level=0x%02x",
			s.StreamKey, len(s.SPS), len(s.PPS), s.NALLengthSize, cfg.Profile, cfg.Level)

		spsInfoJSON, _ := json.MarshalIndent(cfg.SPSInfo, "", "    ")
		log.Printf("[rtmp.session] stream=%s , sps_info=%s", s.StreamKey, spsInfoJSON)
		return nil
	}

	// packet_type == 1 才是真正的 NALU 数据，其余（如 2 = 序列结束）直接忽略
	if info.PacketType != 1 {
		return nil
	}

	// 与 Python 版本一致：这里不再解析一遍 NALU 做完整性校验，
	// 帧数据原样交给上层（真正的解析发生在下发前的参数集重映射里）。
	frame := VideoFrame{
		StreamKey:  s.StreamKey,
		DTS:        info.DTS,
		PTS:        info.PTS,
		IsKeyframe: info.Keyframe,
		AVCCData:   info.Payload,
		RemapCache: flv.NewFrameCache(),
	}
// 	log.Printf("[rtmp.session] frame,dts=%d, pts=%d, avcc_data_len=%d,keyframe=%v",
// 		frame.DTS, frame.PTS, len(frame.AVCCData), frame.IsKeyframe)

	s.VideoCount++

	if s.onVideo != nil {
		s.onVideo(frame, s)
	}
	return nil
}

// handleAudioMessage 处理音频消息。
func (s *Session) handleAudioMessage(timestamp uint32, payload []byte) error {
	info, err := flv.ParseAudioTag(payload, timestamp)
	if err != nil {
		return err
	}

	// packet_type == 0：AAC 序列头（AudioSpecificConfig）
	if info.PacketType == 0 {
		s.AACConfig = info.Payload
		return nil
	}
	if info.PacketType != 1 {
		return nil
	}

	frame := AudioFrame{
		StreamKey: s.StreamKey,
		Timestamp: info.Timestamp,
		Data:      info.Payload,
	}
	s.AudioCount++

	if s.onAudio != nil {
		s.onAudio(frame, s)
	}
	return nil
}

// Run 执行会话主循环：先握手，然后不断读取并分发 RTMP 消息，直到出错或对端断开。
func (s *Session) Run() error {
	// 握手直接写裸连接：S0/S1/S2 必须立刻发出，客户端收到后才会回 C2，
	// 若经过带缓冲的 writer 会造成双方互等而死锁。
	if err := rtmp.ServerHandshake(s.bufReader, s.conn); err != nil {
		return err
	}

	for {
		msg, err := s.reader.ReadMessage()
		if err != nil {
			return err
		}

		if err := s.dispatch(msg); err != nil {
			return err
		}
	}
}

// dispatch 按消息类型分发一条 RTMP 消息。
func (s *Session) dispatch(msg rtmp.Message) error {
	switch msg.TypeID {
	case msgSetChunkSize:
		// 对端通知新的 chunk 大小
		if len(msg.Payload) >= 4 {
			if n := int(beUint32(msg.Payload[:4])); n > 0 && n <= maxChunkSize {
				s.reader.SetChunkSize(n)
			}
		}

	case msgUserControl:
		// PingRequest = 0x0006，需要回应 PingResponse = 0x0007
		if len(msg.Payload) >= 6 && msg.Payload[0] == 0x00 && msg.Payload[1] == 0x06 {
			return s.send(0, msgUserControl, 0,
				append([]byte{0x00, 0x07}, msg.Payload[2:6]...))
		}

	case msgAMF0Command:
		return s.handleCommandMessage(msg.Payload)

	case msgAMF3Command:
		// AMF3 命令消息：首字节为 0 时，后续内容其实是 AMF0 编码
		if len(msg.Payload) > 0 && msg.Payload[0] == 0x00 {
			return s.handleCommandMessage(msg.Payload[1:])
		}

	case msgVideo:
		if s.Publishing {
			return s.handleVideoMessage(msg.Timestamp, msg.Payload)
		}

	case msgAudio:
		if s.Publishing {
			return s.handleAudioMessage(msg.Timestamp, msg.Payload)
		}

	case msgMetadata:
		// 元数据这里有意忽略；后续如有需要可以通过回调暴露出去，
		// 不影响音视频数据的处理。
	}
	return nil
}

// Close 关闭会话（当前无额外资源需要释放，保留以对齐 Python 版本的接口）。
func (s *Session) Close() error { return nil }

// asString 把任意 AMF0 值转成字符串，等价于 Python 的 str()。
func asString(v any) string {
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprintf("%v", v)
}

// be32 返回大端 uint32 字节序列。
func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// beUint32 解析 4 字节大端整数。
func beUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
