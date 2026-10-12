// Package flv 提供 FLV / H.264 相关的解析与生成能力。
//
// parse.go 对应 Python 版本的 rtmp_server/flv.py：
// 负责解析 RTMP 传来的音视频 Tag、AVCDecoderConfigurationRecord 以及 SPS。
package flv

import (
	"encoding/binary"
	"fmt"
)

// -----------------------------------------------------------------------------
// 比特级读取工具（解析 SPS 需要按位读取指数哥伦布码）
// -----------------------------------------------------------------------------

// bitReader 以「位」为单位顺序读取字节切片。
type bitReader struct {
	data   []byte
	bitPos int
}

// newBitReader 创建一个从第 0 位开始的比特读取器。
func newBitReader(data []byte) *bitReader {
	return &bitReader{data: data}
}

// readBit 读取 1 个比特。
func (br *bitReader) readBit() (uint32, error) {
	if br.bitPos >= len(br.data)*8 {
		return 0, fmt.Errorf("flv: SPS 比特流已耗尽")
	}

	bytePos := br.bitPos >> 3
	bitOffset := 7 - (br.bitPos & 7)
	br.bitPos++

	return uint32(br.data[bytePos]>>bitOffset) & 1, nil
}

// readBits 读取 n 个比特并按大端组合成整数。
func (br *bitReader) readBits(n int) (uint32, error) {
	value := uint32(0)
	for i := 0; i < n; i++ {
		bit, err := br.readBit()
		if err != nil {
			return 0, err
		}
		value = value<<1 | bit
	}
	return value, nil
}

// readUE 读取无符号指数哥伦布码（ue(v)）。
func (br *bitReader) readUE() (uint32, error) {
	zeros := 0
	for {
		bit, err := br.readBit()
		if err != nil {
			return 0, err
		}
		if bit == 1 {
			break
		}
		zeros++
	}

	if zeros == 0 {
		return 0, nil
	}

	rest, err := br.readBits(zeros)
	if err != nil {
		return 0, err
	}
	return (1 << zeros) - 1 + rest, nil
}

// readSE 读取有符号指数哥伦布码（se(v)）。
func (br *bitReader) readSE() (int32, error) {
	codeNum, err := br.readUE()
	if err != nil {
		return 0, err
	}
	if codeNum&1 == 1 {
		return int32((codeNum + 1) / 2), nil
	}
	return -int32(codeNum / 2), nil
}

// -----------------------------------------------------------------------------
// H.264 SPS 解析
// -----------------------------------------------------------------------------

// removeEmulationPreventionBytes 去除 H.264 RBSP 中的防竞争字节 0x03。
//
// 例如：00 00 03 01 -> 00 00 01
func removeEmulationPreventionBytes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	zeroCount := 0

	for _, b := range data {
		if zeroCount >= 2 && b == 0x03 {
			zeroCount = 0
			continue
		}
		out = append(out, b)

		if b == 0 {
			zeroCount++
		} else {
			zeroCount = 0
		}
	}
	return out
}

// SPSInfo 是 SPS 解析结果，字段与 Python 版 parse_h264_sps 返回的字典一一对应。
type SPSInfo struct {
	ProfileIDC      uint32 `json:"profile_idc"`
	LevelIDC        uint32 `json:"level_idc"`
	ChromaFormatIDC uint32 `json:"chroma_format_idc"`

	Width  int `json:"width"`
	Height int `json:"height"`

	PicWidthInMbsMinus1       uint32 `json:"pic_width_in_mbs_minus1"`
	PicHeightInMapUnitsMinus1 uint32 `json:"pic_height_in_map_units_minus1"`

	FrameMbsOnlyFlag uint32 `json:"frame_mbs_only_flag"`

	FrameCroppingFlag uint32 `json:"frame_cropping_flag"`
	CropLeft          uint32 `json:"crop_left"`
	CropRight         uint32 `json:"crop_right"`
	CropTop           uint32 `json:"crop_top"`
	CropBottom        uint32 `json:"crop_bottom"`
}

// highProfiles 是带有 chroma_format_idc 等扩展字段的 profile 集合。
var highProfiles = map[uint32]bool{
	100: true, 110: true, 122: true, 244: true,
	44: true, 83: true, 86: true, 118: true,
	128: true, 138: true, 139: true, 134: true,
	135: true,
}

// ParseH264SPS 解析 H.264 SPS，得到视频宽高等信息。
//
// sps 为不含起始码的 SPS NALU，例如：67 64 00 1f ...
func ParseH264SPS(sps []byte) (SPSInfo, error) {
	if len(sps) == 0 {
		return SPSInfo{}, fmt.Errorf("flv: SPS 为空")
	}

	// SPS NALU header：低 5 位为 nal_unit_type，SPS 固定为 7
	if nalType := sps[0] & 0x1f; nalType != 7 {
		return SPSInfo{}, fmt.Errorf("flv: 不是 SPS NALU, nal_type=%d", nalType)
	}

	br := newBitReader(removeEmulationPreventionBytes(sps[1:]))

	// 出错即返回的辅助闭包，避免每一步都写一遍 if err != nil。
	var err error
	bits := func(n int) uint32 {
		if err != nil {
			return 0
		}
		var v uint32
		v, err = br.readBits(n)
		return v
	}
	bit := func() uint32 { return bits(1) }
	ue := func() uint32 {
		if err != nil {
			return 0
		}
		var v uint32
		v, err = br.readUE()
		return v
	}
	se := func() int32 {
		if err != nil {
			return 0
		}
		var v int32
		v, err = br.readSE()
		return v
	}

	profileIDC := bits(8)
	_ = bits(8) // constraint_set_flags + reserved_zero_2bits
	levelIDC := bits(8)
	_ = ue() // seq_parameter_set_id

	// ---------------- High profile 特有字段 ----------------
	chromaFormatIDC := uint32(1)
	if highProfiles[profileIDC] {
		chromaFormatIDC = ue()

		if chromaFormatIDC == 3 {
			_ = bit() // separate_colour_plane_flag
		}

		_ = ue()  // bit_depth_luma_minus8
		_ = ue()  // bit_depth_chroma_minus8
		_ = bit() // qpprime_y_zero_transform_bypass_flag

		if seqScalingMatrixPresentFlag := bit(); seqScalingMatrixPresentFlag == 1 {
			scalingCount := 8
			if chromaFormatIDC == 3 {
				scalingCount = 12
			}

			for i := 0; i < scalingCount; i++ {
				if seqScalingListPresentFlag := bit(); seqScalingListPresentFlag == 1 {
					// 4x4 缩放表 16 个值；8x8 缩放表 64 个值
					size := 16
					if i >= 6 {
						size = 64
					}

					lastScale, nextScale := int32(8), int32(8)
					for j := 0; j < size; j++ {
						if nextScale != 0 {
							deltaScale := se()
							nextScale = (lastScale + deltaScale + 256) % 256
						}
						if nextScale != 0 {
							lastScale = nextScale
						}
					}
				}
			}
		}
	}

	// ---------------- SPS 核心字段 ----------------
	_ = ue() // log2_max_frame_num_minus4

	switch picOrderCntType := ue(); picOrderCntType {
	case 0:
		_ = ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		_ = bit() // delta_pic_order_always_zero_flag
		_ = se()  // offset_for_non_ref_pic
		_ = se()  // offset_for_top_to_bottom_field

		numRefFramesInPicOrderCntCycle := ue()
		for i := uint32(0); i < numRefFramesInPicOrderCntCycle && err == nil; i++ {
			_ = se()
		}
	}

	_ = ue()  // max_num_ref_frames
	_ = bit() // gaps_in_frame_num_value_allowed_flag

	// ---------------- 宽高核心字段 ----------------
	picWidthInMbsMinus1 := ue()
	picHeightInMapUnitsMinus1 := ue()
	frameMbsOnlyFlag := bit()

	if frameMbsOnlyFlag == 0 {
		_ = bit() // mb_adaptive_frame_field_flag
	}
	_ = bit() // direct_8x8_inference_flag

	frameCroppingFlag := bit()
	var cropLeft, cropRight, cropTop, cropBottom uint32
	if frameCroppingFlag == 1 {
		cropLeft = ue()
		cropRight = ue()
		cropTop = ue()
		cropBottom = ue()
	}

	if err != nil {
		return SPSInfo{}, fmt.Errorf("flv: 解析 SPS 失败: %w", err)
	}

	// ---------------- 计算实际分辨率 ----------------
	width := int(picWidthInMbsMinus1+1) * 16
	height := int(2-frameMbsOnlyFlag) * int(picHeightInMapUnitsMinus1+1) * 16

	// 裁剪单位随色度采样格式变化
	cropUnitX, cropUnitY := 1, int(2-frameMbsOnlyFlag)
	switch chromaFormatIDC {
	case 1:
		cropUnitX, cropUnitY = 2, 2*int(2-frameMbsOnlyFlag)
	case 2:
		cropUnitX, cropUnitY = 2, int(2-frameMbsOnlyFlag)
	}

	width -= int(cropLeft+cropRight) * cropUnitX
	height -= int(cropTop+cropBottom) * cropUnitY

	return SPSInfo{
		ProfileIDC:                profileIDC,
		LevelIDC:                  levelIDC,
		ChromaFormatIDC:           chromaFormatIDC,
		Width:                     width,
		Height:                    height,
		PicWidthInMbsMinus1:       picWidthInMbsMinus1,
		PicHeightInMapUnitsMinus1: picHeightInMapUnitsMinus1,
		FrameMbsOnlyFlag:          frameMbsOnlyFlag,
		FrameCroppingFlag:         frameCroppingFlag,
		CropLeft:                  cropLeft,
		CropRight:                 cropRight,
		CropTop:                   cropTop,
		CropBottom:                cropBottom,
	}, nil
}

// -----------------------------------------------------------------------------
// AVCDecoderConfigurationRecord 解析
// -----------------------------------------------------------------------------

// AVCConfig 是 AVCDecoderConfigurationRecord 的解析结果。
type AVCConfig struct {
	Version       uint8
	Profile       uint8
	Compatibility uint8
	Level         uint8
	NALLengthSize int
	SPS           []byte // 第一个 SPS
	PPS           []byte // 第一个 PPS
	SPSInfo       SPSInfo
	SPSList       [][]byte
	PPSList       [][]byte
}

// ParseAVCConfig 解析 AVC 序列头（AVCDecoderConfigurationRecord）。
func ParseAVCConfig(config []byte) (AVCConfig, error) {
	if len(config) < 7 {
		return AVCConfig{}, fmt.Errorf("flv: 非法的 AVCDecoderConfigurationRecord")
	}

	p := 0
	version, profile, compatibility, level := config[0], config[1], config[2], config[3]
	nalLengthSize := int(config[4]&3) + 1
	numSPS := int(config[5] & 0x1f)
	p = 6

	// readParameterSets 连续读取 count 个「2 字节长度 + 数据」结构。
	readParameterSets := func(count int) ([][]byte, error) {
		sets := make([][]byte, 0, count)
		for i := 0; i < count; i++ {
			if p+2 > len(config) {
				return nil, fmt.Errorf("flv: AVC 配置数据被截断")
			}
			n := int(binary.BigEndian.Uint16(config[p : p+2]))
			p += 2

			if p+n > len(config) {
				return nil, fmt.Errorf("flv: AVC 配置数据被截断")
			}
			sets = append(sets, config[p:p+n])
			p += n
		}
		return sets, nil
	}

	spsList, err := readParameterSets(numSPS)
	if err != nil {
		return AVCConfig{}, err
	}

	if p >= len(config) {
		return AVCConfig{}, fmt.Errorf("flv: AVC 配置数据被截断")
	}
	numPPS := int(config[p])
	p++

	ppsList, err := readParameterSets(numPPS)
	if err != nil {
		return AVCConfig{}, err
	}

	if len(spsList) == 0 || len(ppsList) == 0 {
		return AVCConfig{}, fmt.Errorf("flv: AVC 配置中缺少 SPS/PPS")
	}

	spsInfo, err := ParseH264SPS(spsList[0])
	if err != nil {
		return AVCConfig{}, err
	}

	return AVCConfig{
		Version:       version,
		Profile:       profile,
		Compatibility: compatibility,
		Level:         level,
		NALLengthSize: nalLengthSize,
		SPS:           spsList[0],
		PPS:           ppsList[0],
		SPSInfo:       spsInfo,
		SPSList:       spsList,
		PPSList:       ppsList,
	}, nil
}

// -----------------------------------------------------------------------------
// NALU 相关工具
// -----------------------------------------------------------------------------

// ParseAVCNALUs 把 AVCC 格式（[长度][NALU]...）拆分成 NALU 列表。
func ParseAVCNALUs(payload []byte, nalLengthSize int) ([][]byte, error) {
	out := make([][]byte, 0, 4)

	for p := 0; p < len(payload); {
		if p+nalLengthSize > len(payload) {
			return nil, fmt.Errorf("flv: NAL 长度字段被截断")
		}

		n := 0
		for _, b := range payload[p : p+nalLengthSize] {
			n = n<<8 | int(b)
		}
		p += nalLengthSize

		if p+n > len(payload) {
			return nil, fmt.Errorf("flv: NAL 数据被截断")
		}
		if n > 0 {
			out = append(out, payload[p:p+n])
		}
		p += n
	}
	return out, nil
}

// NALUsToAnnexB 把 NALU 列表拼成 Annex-B 格式（每个 NALU 前加 00 00 00 01）。
func NALUsToAnnexB(nalus [][]byte) []byte {
	out := make([]byte, 0, 4*len(nalus))
	for _, nalu := range nalus {
		out = append(out, 0x00, 0x00, 0x00, 0x01)
		out = append(out, nalu...)
	}
	return out
}

// NALUsToAVCC 把多个 NALU 按 AVCC 格式组合：[NALU 长度][NALU][NALU 长度][NALU]...
//
// nalLengthSize 为长度字段字节数，只能是 1、2 或 4。
func NALUsToAVCC(nalus [][]byte, nalLengthSize int) ([]byte, error) {
	if nalLengthSize != 1 && nalLengthSize != 2 && nalLengthSize != 4 {
		return nil, fmt.Errorf("flv: nal_length_size 只能是 1、2 或 4")
	}

	maxNALUSize := (1 << (8 * nalLengthSize)) - 1

	out := make([]byte, 0, 64)
	for _, nalu := range nalus {
		if len(nalu) > maxNALUSize {
			return nil, fmt.Errorf("flv: NALU 大小 %d 超出 %d 字节长度字段的范围",
				len(nalu), nalLengthSize)
		}

		// 大端序写入 NALU 长度
		for shift := (nalLengthSize - 1) * 8; shift >= 0; shift -= 8 {
			out = append(out, byte(len(nalu)>>shift))
		}
		out = append(out, nalu...)
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// FLV Tag 解析
// -----------------------------------------------------------------------------

// VideoTag 是一个视频 Tag 的解析结果。
type VideoTag struct {
	Keyframe        bool   // 是否关键帧
	PacketType      uint8  // 0=序列头 1=NALU 2=序列结束
	CompositionTime int32  // CTS（有符号）
	DTS             int64  // 解码时间戳
	PTS             int64  // 显示时间戳 = DTS + CTS
	Payload         []byte // 去掉 5 字节 Tag 头后的数据
}

// ParseVideoTag 解析 RTMP 视频消息（仅支持 AVC/H.264）。
func ParseVideoTag(data []byte, timestamp uint32) (VideoTag, error) {
	if len(data) < 5 {
		return VideoTag{}, fmt.Errorf("flv: AVC Tag 过短")
	}

	first := data[0]
	if codecID := first & 0x0f; codecID != 7 {
		return VideoTag{}, fmt.Errorf("flv: 不是 AVC 编码, codec_id=%d", codecID)
	}

	// CTS 是 24 位有符号数，需要做符号扩展
	cts := int32(data[2])<<16 | int32(data[3])<<8 | int32(data[4])
	if cts&0x800000 != 0 {
		cts -= 0x1000000
	}

	return VideoTag{
		Keyframe:        first>>4 == 1,
		PacketType:      data[1],
		CompositionTime: cts,
		DTS:             int64(timestamp),
		PTS:             int64(timestamp) + int64(cts),
		Payload:         data[5:],
	}, nil
}

// AudioTag 是一个音频 Tag 的解析结果。
type AudioTag struct {
	PacketType uint8  // 0=序列头(AAC ASC) 1=裸 AAC 数据
	Timestamp  int64  // 时间戳
	Payload    []byte // 去掉 2 字节 Tag 头后的数据
}

// ParseAudioTag 解析 RTMP 音频消息（仅支持 AAC）。
func ParseAudioTag(data []byte, timestamp uint32) (AudioTag, error) {
	if len(data) < 2 {
		return AudioTag{}, fmt.Errorf("flv: AAC Tag 过短")
	}
	if soundFormat := data[0] >> 4; soundFormat != 10 {
		return AudioTag{}, fmt.Errorf("flv: 不是 AAC 编码, sound_format=%d", soundFormat)
	}

	return AudioTag{
		PacketType: data[1],
		Timestamp:  int64(timestamp),
		Payload:    data[2:],
	}, nil
}
