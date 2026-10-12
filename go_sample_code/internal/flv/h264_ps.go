// h264_ps.go 对应 Python 版本的 rtmp_server/h264_ps.py：
// H.264 参数集(SPS/PPS)重编号工具。
//
// 用于 dual-stream 场景：同一路 FLV 输出里会混合 main_stream 与 aux_stream
// 的视频帧。两路码流各自有独立的 SPS/PPS，但它们几乎总是都使用
// seq_parameter_set_id = 0 / pic_parameter_set_id = 0，所以：
//
//   - 只写 main 的参数集 -> aux 的帧解不出来
//   - 只写 aux  的参数集 -> main 的帧解不出来
//   - 两份都写进 AVCDecoderConfigurationRecord -> id 相同，后者覆盖前者
//
// 解决办法（本文件做的事）：
//
//  1. 把 main 的参数集重编号为 sps_id=0 / pps_id=0
//  2. 把 aux  的参数集重编号为 sps_id=1 / pps_id=1
//  3. 两份参数集一起写进 FLV 的 AVC sequence header
//     （AVCDecoderConfigurationRecord 支持 numOfSPS<=31 / numOfPPS<=255）
//  4. 发送每一帧之前，按照它来自哪一路，改写 slice header 里的
//     pic_parameter_set_id，也就是「指定这一帧用哪一份 pps/sps」
//
// 注意：H.264 里「用哪一份 pps」是写在 slice header 里的，容器层
// (FLV/MP4)没有任何字段可以表达这件事，所以第 4 步的比特级改写是
// 这个方案里不可省略的部分。
//
// 说明：Go 版本把这部分代码放在 flv 包内（而不是像 Python 那样单独一个模块），
// 是因为它需要复用本包的比特读取器，而 writer.go 又要用它生成
// AVCDecoderConfigurationRecord —— 拆成独立包会形成循环依赖。

package flv

import (
	"bytes"
	"fmt"
	"math/bits"
	"sync"
)

// 约定的参数集编号。
const (
	MainSPSID = 0
	MainPPSID = 0
	AuxSPSID  = 1
	AuxPPSID  = 1
)

// NALU 类型。
const (
	nalTypeSliceNonIDR = 1 // VCL slice（非 IDR、非数据分区）
	nalTypeSliceIDR    = 5 // VCL slice（IDR）
	nalTypeSPS         = 7
	nalTypePPS         = 8
)

// keepUE 用在改写 ue(v) 字段时表示「保持原值」，等价于 Python 里的 None。
const keepUE = -1

// Logger 是打印日志的回调，等价于 Python 的 print_to_logger。
type Logger func(args ...any)

// -----------------------------------------------------------------------------
// 按位写入
// -----------------------------------------------------------------------------

// bitWriter 按位写入，内部直接维护字节缓冲区，末尾不足一个字节的部分补 0。
type bitWriter struct {
	buf   []byte
	nbits int
}

// writeBit 写入 1 个比特。
func (bw *bitWriter) writeBit(b byte) {
	if bw.nbits&7 == 0 {
		bw.buf = append(bw.buf, 0)
	}
	if b != 0 {
		bw.buf[bw.nbits>>3] |= 1 << uint(7-(bw.nbits&7))
	}
	bw.nbits++
}

// writeByte 一次写入 8 个比特（跨字节边界时自动拆分），避免逐比特循环。
func (bw *bitWriter) writeByte(b byte) {
	if off := bw.nbits & 7; off == 0 {
		bw.buf = append(bw.buf, b)
	} else {
		bw.buf[len(bw.buf)-1] |= b >> uint(off)
		bw.buf = append(bw.buf, b<<uint(8-off))
	}
	bw.nbits += 8
}

// writeBits 写入 value 的低 n 位（高位在前）。
func (bw *bitWriter) writeBits(value uint64, n int) {
	if n <= 0 {
		return
	}
	for i := n - 1; i >= 0; i-- {
		bw.writeBit(byte((value >> uint(i)) & 1))
	}
}

// writeUE 写入一个无符号指数哥伦布码 ue(v)。
func (bw *bitWriter) writeUE(value uint32) {
	code := uint64(value) + 1
	n := bits.Len64(code)
	bw.writeBits(0, n-1)
	bw.writeBits(code, n)
}

// copyBits 从 src 的第 startBit 位开始搬运 n 个比特。
func (bw *bitWriter) copyBits(src []byte, startBit, n int) {
	for n >= 8 {
		bw.writeByte(byteAtBit(src, startBit))
		startBit += 8
		n -= 8
	}
	for ; n > 0; n-- {
		bw.writeBit(byte(src[startBit>>3]>>uint(7-(startBit&7))) & 1)
		startBit++
	}
}

// byteAtBit 读取 src 中从任意比特位置开始的 8 个比特。
func byteAtBit(src []byte, startBit int) byte {
	i := startBit >> 3
	off := uint(startBit & 7)

	window := uint16(src[i]) << 8
	if i+1 < len(src) {
		window |= uint16(src[i+1])
	}
	return byte(window >> (8 - off))
}

// -----------------------------------------------------------------------------
// emulation prevention byte
// -----------------------------------------------------------------------------

// UnescapeRBSP 去掉 emulation prevention byte(0x03)，NALU payload -> RBSP。
//
// 与 removeEmulationPreventionBytes 等价，但用 Index 批量搬运，避免逐字节循环。
func UnescapeRBSP(data []byte) []byte {
	if !bytes.Contains(data, []byte{0x00, 0x00, 0x03}) {
		return data
	}

	out := make([]byte, 0, len(data))
	start, i := 0, 0
	for {
		rel := bytes.Index(data[i:], []byte{0x00, 0x00, 0x03})
		if rel < 0 {
			break
		}
		j := i + rel

		out = append(out, data[start:j+2]...)
		// 0x03 被丢弃，同时零计数器复位，下一个可能的转义最早出现在 j+3
		start = j + 3
		i = j + 3
	}
	return append(out, data[start:]...)
}

// EscapeRBSP 把 RBSP 转回 NALU payload，在 00 00 0x (x<=3) 前插入 0x03。
func EscapeRBSP(data []byte) []byte {
	if !bytes.Contains(data, []byte{0x00, 0x00}) {
		return data
	}

	out := make([]byte, 0, len(data)+8)
	start, i, n := 0, 0, len(data)
	for {
		rel := bytes.Index(data[i:], []byte{0x00, 0x00})
		if rel < 0 {
			break
		}
		j := i + rel
		if j+2 >= n {
			break
		}

		if data[j+2] <= 3 {
			out = append(out, data[start:j+2]...)
			out = append(out, 0x03)
			start = j + 2
			// 插入转义后零计数器复位，下一个 00 00 最早从 j+2 开始
			i = j + 2
		} else {
			// data[j+2] 非零，零计数器复位
			i = j + 3
		}
	}
	return append(out, data[start:]...)
}

// stripTrailingZeroBytes 去掉 rbsp_stop_one_bit 之后多余的全零字节。
//
// 改写 ue 字段会让整个 RBSP 向后平移若干比特，补齐字节边界后末尾可能多出
// 一个 0x00。H.264 要求 NALU 最后一个字节不为 0x00，而 stop bit 之后的零
// 全部是可选的 trailing_zero_8bits，直接丢掉即可。
func stripTrailingZeroBytes(rbsp []byte) []byte {
	end := len(rbsp)
	for end > 1 && rbsp[end-1] == 0 {
		end--
	}
	return rbsp[:end]
}

// -----------------------------------------------------------------------------
// 核心：改写 RBSP 开头的若干个 ue(v) 字段
// -----------------------------------------------------------------------------

// rewriteLeadingUE 改写 RBSP 开头的 ue(v) 字段。
//
//	rbsp       已经去掉 NALU header 和转义字节的 RBSP
//	prefixBits ue 字段之前需要原样保留的比特数（SPS 是 24：
//	           profile_idc + constraint_flags + level_idc）
//	newValues  与前 len(newValues) 个 ue 字段一一对应的新值，
//	           keepUE 表示保持原值
//
// 返回的 changed 为 false 时表示所有字段都没变化，此时直接返回入参切片。
func rewriteLeadingUE(rbsp []byte, prefixBits int, newValues []int) (out []byte, changed bool, err error) {
	br := newBitReader(rbsp)

	prefix := uint32(0)
	if prefixBits > 0 {
		if prefix, err = br.readBits(prefixBits); err != nil {
			return nil, false, err
		}
	}

	olds := make([]uint32, len(newValues))
	for i := range newValues {
		if olds[i], err = br.readUE(); err != nil {
			return nil, false, err
		}
	}
	consumed := br.bitPos

	news := make([]uint32, len(newValues))
	same := true
	for i, v := range newValues {
		if v == keepUE {
			news[i] = olds[i]
		} else {
			news[i] = uint32(v)
		}
		if news[i] != olds[i] {
			same = false
		}
	}
	if same {
		return rbsp, false, nil
	}

	bw := &bitWriter{buf: make([]byte, 0, len(rbsp)+4)}
	bw.writeBits(uint64(prefix), prefixBits)
	for _, v := range news {
		bw.writeUE(v)
	}
	// 剩余比特整体搬运
	bw.copyBits(rbsp, consumed, len(rbsp)*8-consumed)

	return stripTrailingZeroBytes(bw.buf), true, nil
}

// rewriteNALU 是带 NALU header / 转义处理的 rewriteLeadingUE。
func rewriteNALU(nalu []byte, prefixBits int, newValues []int) ([]byte, error) {
	if len(nalu) < 1 {
		return nil, fmt.Errorf("flv: NALU 为空")
	}

	rbsp := UnescapeRBSP(nalu[1:])
	newRBSP, changed, err := rewriteLeadingUE(rbsp, prefixBits, newValues)
	if err != nil {
		return nil, err
	}
	if !changed {
		return nalu, nil
	}

	escaped := EscapeRBSP(newRBSP)
	out := make([]byte, 0, 1+len(escaped))
	out = append(out, nalu[0])
	return append(out, escaped...), nil
}

// -----------------------------------------------------------------------------
// SPS / PPS / slice
// -----------------------------------------------------------------------------

// GetSPSID 读取 SPS 的 seq_parameter_set_id。
func GetSPSID(sps []byte) (uint32, error) {
	if len(sps) < 1 {
		return 0, fmt.Errorf("flv: SPS 为空")
	}

	br := newBitReader(UnescapeRBSP(sps[1:]))
	if _, err := br.readBits(24); err != nil { // profile_idc + constraint_flags + level_idc
		return 0, err
	}
	return br.readUE()
}

// SetSPSID 改写 SPS 的 seq_parameter_set_id。
func SetSPSID(sps []byte, spsID int) ([]byte, error) {
	return rewriteNALU(sps, 24, []int{spsID})
}

// GetPPSIDs 读取 PPS 的 (pic_parameter_set_id, seq_parameter_set_id)。
func GetPPSIDs(pps []byte) (ppsID, spsID uint32, err error) {
	if len(pps) < 1 {
		return 0, 0, fmt.Errorf("flv: PPS 为空")
	}

	br := newBitReader(UnescapeRBSP(pps[1:]))
	if ppsID, err = br.readUE(); err != nil {
		return 0, 0, err
	}
	if spsID, err = br.readUE(); err != nil {
		return 0, 0, err
	}
	return ppsID, spsID, nil
}

// SetPPSIDs 改写 PPS 的 pic_parameter_set_id / seq_parameter_set_id。
func SetPPSIDs(pps []byte, ppsID, spsID int) ([]byte, error) {
	return rewriteNALU(pps, 0, []int{ppsID, spsID})
}

// GetSlicePPSID 读取 slice header 里的 pic_parameter_set_id。
//
//	slice_header():
//	    first_mb_in_slice     ue(v)
//	    slice_type            ue(v)
//	    pic_parameter_set_id  ue(v)
func GetSlicePPSID(nalu []byte) (uint32, error) {
	if len(nalu) < 1 {
		return 0, fmt.Errorf("flv: slice NALU 为空")
	}

	br := newBitReader(UnescapeRBSP(nalu[1:]))
	if _, err := br.readUE(); err != nil { // first_mb_in_slice
		return 0, err
	}
	if _, err := br.readUE(); err != nil { // slice_type
		return 0, err
	}
	return br.readUE()
}

// SetSlicePPSID 改写 slice header 里的 pic_parameter_set_id。
func SetSlicePPSID(nalu []byte, ppsID int) ([]byte, error) {
	return rewriteNALU(nalu, 0, []int{keepUE, keepUE, ppsID})
}

// -----------------------------------------------------------------------------
// AVCC 帧级别的重映射
// -----------------------------------------------------------------------------

// IterAVCCNALUs 遍历 AVCC (length-prefixed) 数据里的每个 NALU。
//
// 与 ParseAVCNALUs 的区别：遇到非法长度时直接停止，不返回错误
// （对应 Python 生成器 iter_avcc_nalus 的 break 行为）。
func IterAVCCNALUs(avcc []byte, nalLengthSize int) [][]byte {
	out := make([][]byte, 0, 4)

	p, total := 0, len(avcc)
	for p+nalLengthSize <= total {
		n := 0
		for _, b := range avcc[p : p+nalLengthSize] {
			n = n<<8 | int(b)
		}
		p += nalLengthSize

		if n == 0 || p+n > total {
			break
		}
		out = append(out, avcc[p:p+n])
		p += n
	}
	return out
}

// nalusToAVCC 把 NALU 列表拼成 AVCC 格式（不做长度校验，对应 Python 的 nalus_to_avcc）。
func nalusToAVCC(nalus [][]byte, nalLengthSize int) []byte {
	size := 0
	for _, nalu := range nalus {
		size += nalLengthSize + len(nalu)
	}

	out := make([]byte, 0, size)
	for _, nalu := range nalus {
		for shift := (nalLengthSize - 1) * 8; shift >= 0; shift -= 8 {
			out = append(out, byte(len(nalu)>>shift))
		}
		out = append(out, nalu...)
	}
	return out
}

// isIdentityMap 判断映射表是否为空或所有键都映射到自身。
func isIdentityMap(m map[uint32]int) bool {
	for k, v := range m {
		if int(k) != v {
			return false
		}
	}
	return true
}

// RemapAVCCFrame 把一帧 AVCC 数据里所有 slice 的 pic_parameter_set_id 重映射。
//
// 顺带把帧内携带的 SPS/PPS NALU（有些编码器每个 IDR 前都带）一起重编号，
// 否则它们会把 sequence header 里的参数集覆盖掉。
//
// ppsIDMap 里找不到的 id 保持原样。
func RemapAVCCFrame(avcc []byte, ppsIDMap, spsIDMap map[uint32]int, nalLengthSize int) ([]byte, error) {
	if isIdentityMap(ppsIDMap) && isIdentityMap(spsIDMap) {
		return avcc, nil
	}

	nalus := IterAVCCNALUs(avcc, nalLengthSize)
	out := make([][]byte, 0, len(nalus))
	changed := false

	for _, nalu := range nalus {
		newNALU, naluChanged := nalu, false

		switch nalu[0] & 0x1f {
		case nalTypeSliceNonIDR, nalTypeSliceIDR:
			old, err := GetSlicePPSID(nalu)
			if err != nil {
				return nil, err
			}
			if to, ok := ppsIDMap[old]; ok && to != int(old) {
				if newNALU, err = SetSlicePPSID(nalu, to); err != nil {
					return nil, err
				}
				naluChanged = true
			}

		case nalTypeSPS:
			old, err := GetSPSID(nalu)
			if err != nil {
				return nil, err
			}
			if to, ok := spsIDMap[old]; ok && to != int(old) {
				if newNALU, err = SetSPSID(nalu, to); err != nil {
					return nil, err
				}
				naluChanged = true
			}

		case nalTypePPS:
			oldPPS, oldSPS, err := GetPPSIDs(nalu)
			if err != nil {
				return nil, err
			}
			newPPS, newSPS := int(oldPPS), int(oldSPS)
			if to, ok := ppsIDMap[oldPPS]; ok {
				newPPS = to
			}
			if to, ok := spsIDMap[oldSPS]; ok {
				newSPS = to
			}
			if newPPS != int(oldPPS) || newSPS != int(oldSPS) {
				if newNALU, err = SetPPSIDs(nalu, newPPS, newSPS); err != nil {
					return nil, err
				}
				naluChanged = true
			}
		}

		if naluChanged {
			changed = true
		}
		out = append(out, newNALU)
	}

	if !changed {
		return avcc, nil
	}
	return nalusToAVCC(out, nalLengthSize), nil
}

// -----------------------------------------------------------------------------
// AVCDecoderConfigurationRecord
// -----------------------------------------------------------------------------

// BuildAVCDecoderConfig 生成 AVCDecoderConfigurationRecord，支持多份 SPS/PPS。
func BuildAVCDecoderConfig(spsList, ppsList [][]byte, nalLengthSize int) ([]byte, error) {
	spsList = nonEmpty(spsList)
	ppsList = nonEmpty(ppsList)

	if len(spsList) == 0 || len(ppsList) == 0 {
		return nil, fmt.Errorf("flv: AVC config needs at least one SPS and one PPS")
	}
	if len(spsList) > 31 {
		return nil, fmt.Errorf("flv: too many SPS (max 31)")
	}
	if len(ppsList) > 255 {
		return nil, fmt.Errorf("flv: too many PPS (max 255)")
	}
	if len(spsList[0]) < 4 {
		return nil, fmt.Errorf("flv: SPS 过短，无法取出 profile/level")
	}

	// profile/compat 取第一份 SPS，level 取最大值（配置记录描述的是整体能力）
	profile, compatibility := spsList[0][1], spsList[0][2]
	level := spsList[0][3]
	for _, sps := range spsList[1:] {
		if len(sps) < 4 {
			return nil, fmt.Errorf("flv: SPS 过短，无法取出 profile/level")
		}
		if sps[3] > level {
			level = sps[3]
		}
	}

	out := make([]byte, 0, 16)
	out = append(out,
		0x01,          // configurationVersion
		profile,       // AVCProfileIndication
		compatibility, // profile_compatibility
		level,         // AVCLevelIndication
		byte(0xFC|(nalLengthSize-1)),
		byte(0xE0|len(spsList)), // numOfSequenceParameterSets
	)
	for _, sps := range spsList {
		out = append(out, be16(uint16(len(sps)))...)
		out = append(out, sps...)
	}

	out = append(out, byte(len(ppsList))) // numOfPictureParameterSets
	for _, pps := range ppsList {
		out = append(out, be16(uint16(len(pps)))...)
		out = append(out, pps...)
	}
	return out, nil
}

// nonEmpty 过滤掉空的参数集，等价于 Python 的 [_ for _ in list if _]。
func nonEmpty(list [][]byte) [][]byte {
	out := make([][]byte, 0, len(list))
	for _, item := range list {
		if len(item) > 0 {
			out = append(out, item)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// dual-stream 参数集管理
// -----------------------------------------------------------------------------

// FrameCache 缓存一帧在不同目标 pps_id 下的重映射结果。
//
// 同一帧会被多个播放连接使用，缓存可以让比特改写只做一次；
// 对应 Python 中挂在 VideoFrame 上的 remap_cache 字典。
type FrameCache struct {
	mu   sync.Mutex
	data map[int][]byte
}

// NewFrameCache 创建一个空的帧级重映射缓存。
func NewFrameCache() *FrameCache {
	return &FrameCache{data: make(map[int][]byte, 2)}
}

// DualStreamParameterSets 管理 main / aux 两路参数集，并负责逐帧重映射。
//
// 用法：
//
//	ps, _ := flv.NewDualStreamParameterSets(mainSPS, mainPPS, auxSPS, auxPPS, 4, logger)
//	header, _ := flvObj.GetInitFLV(ps.SPSList, ps.PPSList, ps.NALLengthSize)
//	data := ps.Remap(frame.AVCCData, frame.RemapCache, true)  // aux 帧
//	data := ps.Remap(frame.AVCCData, frame.RemapCache, false) // main 帧
type DualStreamParameterSets struct {
	NALLengthSize int
	Dual          bool // aux 是否也有可用的 SPS/PPS

	MainSPSID uint32 // main 原始的 seq_parameter_set_id
	MainPPSID uint32 // main 原始的 pic_parameter_set_id
	AuxSPSID  int    // aux 原始的 seq_parameter_set_id，非 dual 时为 -1
	AuxPPSID  int    // aux 原始的 pic_parameter_set_id，非 dual 时为 -1

	MainSPS []byte // 重编号为 (sps 0) 之后的 main SPS
	MainPPS []byte // 重编号为 (pps 0, sps 0) 之后的 main PPS
	AuxSPS  []byte // 重编号为 (sps 1) 之后的 aux SPS
	AuxPPS  []byte // 重编号为 (pps 1, sps 1) 之后的 aux PPS

	// 两路参数集一起写进 sequence header
	SPSList [][]byte
	PPSList [][]byte

	mainPPSMap map[uint32]int
	mainSPSMap map[uint32]int
	auxPPSMap  map[uint32]int
	auxSPSMap  map[uint32]int

	log Logger
}

// NewDualStreamParameterSets 按 main / aux 的原始参数集构建重编号方案。
//
// auxSPS / auxPPS 为空时退化成单路模式：只写 main 的参数集，
// 帧数据原样下发。
func NewDualStreamParameterSets(mainSPS, mainPPS, auxSPS, auxPPS []byte,
	nalLengthSize int, logger Logger) (*DualStreamParameterSets, error) {

	if len(mainSPS) == 0 || len(mainPPS) == 0 {
		return nil, fmt.Errorf("flv: main stream has no SPS/PPS yet")
	}
	if nalLengthSize < 1 || nalLengthSize > 4 {
		return nil, fmt.Errorf("flv: 非法的 nal_length_size: %d", nalLengthSize)
	}

	d := &DualStreamParameterSets{
		NALLengthSize: nalLengthSize,
		AuxSPSID:      -1,
		AuxPPSID:      -1,
		log:           logger,
	}

	var err error
	if d.MainSPSID, err = GetSPSID(mainSPS); err != nil {
		return nil, err
	}
	if d.MainPPSID, _, err = GetPPSIDs(mainPPS); err != nil {
		return nil, err
	}

	// main -> (0, 0)
	if d.MainSPS, err = SetSPSID(mainSPS, MainSPSID); err != nil {
		return nil, err
	}
	if d.MainPPS, err = SetPPSIDs(mainPPS, MainPPSID, MainSPSID); err != nil {
		return nil, err
	}
	d.mainPPSMap = map[uint32]int{d.MainPPSID: MainPPSID}
	d.mainSPSMap = map[uint32]int{d.MainSPSID: MainSPSID}

	d.Dual = len(auxSPS) > 0 && len(auxPPS) > 0
	if d.Dual {
		auxSPSID, err := GetSPSID(auxSPS)
		if err != nil {
			return nil, err
		}
		auxPPSID, _, err := GetPPSIDs(auxPPS)
		if err != nil {
			return nil, err
		}
		d.AuxSPSID, d.AuxPPSID = int(auxSPSID), int(auxPPSID)

		// aux -> (1, 1)
		if d.AuxSPS, err = SetSPSID(auxSPS, AuxSPSID); err != nil {
			return nil, err
		}
		if d.AuxPPS, err = SetPPSIDs(auxPPS, AuxPPSID, AuxSPSID); err != nil {
			return nil, err
		}
		d.auxPPSMap = map[uint32]int{auxPPSID: AuxPPSID}
		d.auxSPSMap = map[uint32]int{auxSPSID: AuxSPSID}
	} else {
		d.auxPPSMap = map[uint32]int{}
		d.auxSPSMap = map[uint32]int{}
	}

	d.SPSList = [][]byte{d.MainSPS}
	d.PPSList = [][]byte{d.MainPPS}
	if d.Dual {
		d.SPSList = append(d.SPSList, d.AuxSPS)
		d.PPSList = append(d.PPSList, d.AuxPPS)
	}
	return d, nil
}

// String 输出重编号方案，格式与 Python 版本保持一致，便于对照日志。
func (d *DualStreamParameterSets) String() string {
	auxSPS, auxPPS := "None", "None"
	if d.Dual {
		auxSPS, auxPPS = fmt.Sprint(d.AuxSPSID), fmt.Sprint(d.AuxPPSID)
	}

	dual := "False"
	if d.Dual {
		dual = "True"
	}

	return fmt.Sprintf("<DualStreamParameterSets dual=%s main(sps=%d,pps=%d)->(%d,%d) "+
		"aux(sps=%s,pps=%s)->(%d,%d)>",
		dual, d.MainSPSID, d.MainPPSID, MainSPSID, MainPPSID,
		auxSPS, auxPPS, AuxSPSID, AuxPPSID)
}

// InbandParameterSets 返回可以直接插在关键帧前面的 SPS+PPS（AVCC 格式）。
//
// sequence header 里已经有两份参数集了，这里再带一次是为了让切换时刻的
// 「激活」更加确定 —— 部分播放器/硬解只在遇到 in-band 参数集时才刷新
// 自己的参数集表。
func (d *DualStreamParameterSets) InbandParameterSets(aux bool) []byte {
	nalus := [][]byte{d.MainSPS, d.MainPPS}
	if aux {
		if !d.Dual {
			return nil
		}
		nalus = [][]byte{d.AuxSPS, d.AuxPPS}
	}
	return nalusToAVCC(nalus, d.NALLengthSize)
}

// mapFor 返回某一路的 (pps 映射, sps 映射, 目标 pps_id)。
func (d *DualStreamParameterSets) mapFor(aux bool) (ppsMap, spsMap map[uint32]int, target int) {
	if aux {
		return d.auxPPSMap, d.auxSPSMap, AuxPPSID
	}
	return d.mainPPSMap, d.mainSPSMap, MainPPSID
}

// RemapData 对一段 AVCC 数据做重映射（无缓存）。
func (d *DualStreamParameterSets) RemapData(avcc []byte, aux bool) ([]byte, error) {
	ppsMap, spsMap, _ := d.mapFor(aux)
	return RemapAVCCFrame(avcc, ppsMap, spsMap, d.NALLengthSize)
}

// Remap 对一帧做重映射，结果缓存在 cache 上。
//
// 缓存 key 用目标 pps_id，因为帧的来源(即原始 pps_id)是固定的。
// 重映射失败时打印日志并退回原始数据，与 Python 版本一致。
func (d *DualStreamParameterSets) Remap(avcc []byte, cache *FrameCache, aux bool) []byte {
	if len(avcc) == 0 {
		return nil
	}

	_, _, target := d.mapFor(aux)
	if cache == nil {
		data, err := d.RemapData(avcc, aux)
		if err != nil {
			d.logRemapFailed(err)
			return avcc
		}
		return data
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if data, ok := cache.data[target]; ok {
		return data
	}

	data, err := d.RemapData(avcc, aux)
	if err != nil {
		d.logRemapFailed(err)
		data = avcc
	}
	cache.data[target] = data
	return data
}

// logRemapFailed 打印重映射失败日志。
func (d *DualStreamParameterSets) logRemapFailed(err error) {
	if d.log != nil {
		d.log("remap frame failed, use raw data:", err)
	}
}
