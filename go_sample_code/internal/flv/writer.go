// writer.go 对应 Python 版本的 rtmp_server/init_flv.py：
// 负责生成下发给播放器的 FLV 文件头与 FLV 视频 Tag。
//
// 这里刻意保持与 Python 版本「逐字节一致」的输出，包括 onMetaData
// 中的固定字段等；AVCDecoderConfigurationRecord 由 h264_ps.go 生成，
// 支持一次写入 main / aux 两份参数集。

package flv

import (
	"encoding/binary"
	"math"
)

// 生成 FLV 时用到的固定常量。
const (
	// tagTypeScript 是脚本（元数据）Tag 类型。
	tagTypeScript = 0x12
	// tagTypeVideo 是视频 Tag 类型。
	tagTypeVideo = 0x09
	// defaultVideoDataRate 是写入 onMetaData 的码率字段，与 Python 版本一致。
	defaultVideoDataRate = 1024
	// defaultFrameRate 是写入 onMetaData 的帧率字段（0x4038000000000000 = 24.0）。
	defaultFrameRate = 24
	// videoCodecIDAVC 是 onMetaData 中的视频编码 ID（7 = AVC）。
	videoCodecIDAVC = 7
	// encoderName 是写入 onMetaData 的编码器名称，与 Python 版本一致。
	encoderName = "Lavf58.29.100"
)

// flvFileHeader 是 FLV 文件头：'F' 'L' 'V'、版本 1、flags 0x01（仅视频）、头长度 9。
var flvFileHeader = []byte{0x46, 0x4C, 0x56, 0x01, 0x01, 0x00, 0x00, 0x00, 0x09}

// Flv 是生成 FLV 数据的核心类型。
//
// 它本身是只读的（创建后不再修改字段），因此所有方法都是纯函数，
// 可以被多个连接安全地共享调用。
type Flv struct {
	Width         int // 视频宽度
	Height        int // 视频高度
	VideoDataRate int // 码率字段（写入 onMetaData）
}

// NewFlv 按给定的宽高创建 FLV 生成器。
func NewFlv(width, height int) *Flv {
	return &Flv{
		Width:         width,
		Height:        height,
		VideoDataRate: defaultVideoDataRate,
	}
}

// GetInitFLV 生成 FLV 文件头部分，包含：
//
//	FLV Header + PreviousTagSize0 + onMetaData 脚本 Tag + AVC 序列头 Tag
//
// 播放器拿到这段数据后即可完成解码器初始化。
//
// spsList / ppsList 支持一次写入多份参数集（main 一份 + aux 一份），
// 配合 h264_ps.go 把两路的 sps_id/pps_id 错开，这样同一个 FLV 里
// main 帧和 aux 帧都能解码。
func (f *Flv) GetInitFLV(spsList, ppsList [][]byte, nalLengthSize int) ([]byte, error) {
	// 脚本 Tag：onMetaData
	metadataTag := buildTagWithPrevSize(tagTypeScript, 0, f.buildMetadataBody())

	// 视频序列头 Tag：AVCDecoderConfigurationRecord
	sequenceHeader, err := buildAVCSequenceHeader(spsList, ppsList, nalLengthSize)
	if err != nil {
		return nil, err
	}
	sequenceHeaderTag := buildTagWithPrevSize(tagTypeVideo, 0, sequenceHeader)

	out := make([]byte, 0, len(metadataTag)+len(sequenceHeaderTag)+16)
	out = append(out, flvFileHeader...)
	out = append(out, 0x00, 0x00, 0x00, 0x00) // PreviousTagSize0，恒为 0
	out = append(out, metadataTag...)
	out = append(out, sequenceHeaderTag...)
	return out, nil
}

// GetVideo 根据 dts、帧数据与是否关键帧生成一个 FLV 视频 Tag（含 PreviousTagSize）。
//
// frameData 必须是 AVCC 格式（[长度][NALU]...）的裸帧数据。
func (f *Flv) GetVideo(dts uint32, frameData []byte, isKey bool) []byte {
	// Tag 数据头 5 字节：
	//   0x17 = 关键帧 + AVC，0x27 = 非关键帧 + AVC
	//   0x01 = AVC NALU
	//   后 3 字节为 CompositionTime，这里固定为 0
	frameType := byte(0x27)
	if isKey {
		frameType = 0x17
	}

	body := make([]byte, 0, 5+len(frameData))
	body = append(body, frameType, 0x01, 0x00, 0x00, 0x00)
	body = append(body, frameData...)

	return buildTagWithPrevSize(tagTypeVideo, dts, body)
}

// buildMetadataBody 生成 onMetaData 脚本 Tag 的数据部分。
//
// 字段顺序与 Python 版本完全一致，共 8 个字段：
// duration / width / height / videodatarate / framerate / videocodecid /
// encoder / filesize。
func (f *Flv) buildMetadataBody() []byte {
	out := make([]byte, 0, 192)

	// AMF0 字符串 "onMetaData"
	out = append(out, amfString("onMetaData")...)

	// AMF0 ECMA 数组，声明 8 个元素
	out = append(out, 0x08)
	out = append(out, be32(8)...)

	// 各个元数据字段（key 不带类型标记，value 带类型标记）
	out = append(out, amfNumberProperty("duration", 0)...)
	out = append(out, amfNumberProperty("width", float64(f.Width))...)
	out = append(out, amfNumberProperty("height", float64(f.Height))...)
	out = append(out, amfNumberProperty("videodatarate", float64(f.VideoDataRate))...)
	out = append(out, amfNumberProperty("framerate", defaultFrameRate)...)
	out = append(out, amfNumberProperty("videocodecid", videoCodecIDAVC)...)
	out = append(out, amfStringProperty("encoder", encoderName)...)
	out = append(out, amfNumberProperty("filesize", 0)...)

	// ECMA 数组结束标记
	return append(out, 0x00, 0x00, 0x09)
}

// buildAVCSequenceHeader 生成 AVC 序列头 Tag 的数据部分。
//
// profile / compatibility / level 直接取自 SPS，不再写死，
// 否则换了编码器就会对不上。
func buildAVCSequenceHeader(spsList, ppsList [][]byte, nalLengthSize int) ([]byte, error) {
	config, err := BuildAVCDecoderConfig(spsList, ppsList, nalLengthSize)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 5+len(config))

	// 视频 Tag 头：关键帧 + AVC，包类型 0（序列头），CTS = 0
	out = append(out, 0x17, 0x00, 0x00, 0x00, 0x00)
	return append(out, config...), nil
}

// buildTagWithPrevSize 封装一个 FLV Tag，并在其后追加 4 字节 PreviousTagSize。
//
// Tag 头共 11 字节：
//
//	[1] TagType
//	[3] DataSize
//	[3] Timestamp 低 24 位
//	[1] TimestampExtended（时间戳高 8 位）
//	[3] StreamID（恒为 0）
func buildTagWithPrevSize(tagType byte, timestamp uint32, body []byte) []byte {
	out := make([]byte, 0, 15+len(body))

	out = append(out, tagType)
	out = append(out, be24(uint32(len(body)))...)
	out = append(out, be24(timestamp&0xffffff)...)
	out = append(out, byte(timestamp>>24))
	out = append(out, 0x00, 0x00, 0x00)
	out = append(out, body...)

	// PreviousTagSize = Tag 头(11) + 数据长度
	return append(out, be32(uint32(11+len(body)))...)
}

// amfString 编码一个带类型标记的 AMF0 字符串。
func amfString(v string) []byte {
	out := make([]byte, 0, 3+len(v))
	out = append(out, 0x02)
	out = append(out, be16(uint16(len(v)))...)
	return append(out, v...)
}

// amfNumberProperty 编码 ECMA 数组中的一个数值字段（key 无类型标记）。
func amfNumberProperty(key string, value float64) []byte {
	out := make([]byte, 0, 2+len(key)+9)
	out = append(out, be16(uint16(len(key)))...)
	out = append(out, key...)
	out = append(out, 0x00) // number 标记

	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, math.Float64bits(value))
	return append(out, raw...)
}

// amfStringProperty 编码 ECMA 数组中的一个字符串字段（key 无类型标记）。
func amfStringProperty(key, value string) []byte {
	out := make([]byte, 0, 2+len(key)+3+len(value))
	out = append(out, be16(uint16(len(key)))...)
	out = append(out, key...)
	return append(out, amfString(value)...)
}

// be16 返回大端 uint16 字节序列。
func be16(v uint16) []byte {
	return []byte{byte(v >> 8), byte(v)}
}

// be24 返回大端 24 位字节序列。
func be24(v uint32) []byte {
	return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

// be32 返回大端 uint32 字节序列。
func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
