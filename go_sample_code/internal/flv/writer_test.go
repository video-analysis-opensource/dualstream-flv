package flv

import (
	"encoding/hex"
	"testing"
)

// 测试用的 SPS / PPS（取自一路 1920x1080 的 H.264 流）。
const (
	testSPSHex = "67640028acd100f0044fcb35010101400000fa000030d40e8600002625a00004c4b6f8de3c600224000002"
	testPPSHex = "68ee3cb0"
)

// 下面这些期望值由原始 Python 实现（rtmp_server/init_flv.py）直接生成，
// 用于保证 Go 版本的输出与 Python 版本逐字节一致。
const (
	expectedInitFLVHex = "464c5601010000000900000000120000b80000000000000002000a6f6e4d657461446174610800000008" +
		"00086475726174696f6e0000000000000000000005776964746800409e0000000000000006686569676874" +
		"004090e00000000000000d766964656f646174617261746500409000000000000000096672616d65726174" +
		"65004038000000000000000c766964656f636f646563696400401c0000000000000007656e636f64657202" +
		"000d4c61766635382e32392e313030000866696c6573697a65000000000000000000000009000000c30900" +
		"003f00000000000000170000000001640028ffe1002b67640028acd100f0044fcb35010101400000fa0000" +
		"30d40e8600002625a00004c4b6f8de3c60022400000201000468ee3cb00000004a"

	expectedKeyFrameHex   = "0900001102abcd0100000017010000000000000a65888084218ffe120000001c"
	expectedInterFrameHex = "0900000e0004d200000000270100000000000005419a0102ff00000019"
)

// mustDecodeHex 解析十六进制字符串，失败直接让测试报错。
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()

	raw, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("非法的十六进制字符串: %v", err)
	}
	return raw
}

// TestGetInitFLV 校验 FLV 头（含 onMetaData 与 AVC 序列头）与 Python 版本完全一致。
func TestGetInitFLV(t *testing.T) {
	got, err := NewFlv(1920, 1080).GetInitFLV(
		[][]byte{mustDecodeHex(t, testSPSHex)},
		[][]byte{mustDecodeHex(t, testPPSHex)},
		4,
	)
	if err != nil {
		t.Fatalf("GetInitFLV 返回错误: %v", err)
	}

	if want := expectedInitFLVHex; hex.EncodeToString(got) != want {
		t.Fatalf("FLV 头不一致\n got=%s\nwant=%s", hex.EncodeToString(got), want)
	}
}

// TestGetVideo 校验视频 Tag（关键帧与非关键帧）与 Python 版本完全一致。
func TestGetVideo(t *testing.T) {
	cases := []struct {
		name  string
		dts   uint32
		data  string
		isKey bool
		want  string
	}{
		{"关键帧", 0x0102abcd, "0000000a65888084218ffe12", true, expectedKeyFrameHex},
		{"非关键帧", 1234, "00000005419a0102ff", false, expectedInterFrameHex},
	}

	generator := NewFlv(1920, 1080)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generator.GetVideo(tc.dts, mustDecodeHex(t, tc.data), tc.isKey)

			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("视频 Tag 不一致\n got=%s\nwant=%s", hex.EncodeToString(got), tc.want)
			}
		})
	}
}

// TestParseAVCConfigRoundTrip 校验「生成的 AVC 序列头」可以被自己的解析器正确解析回来，
// 并且从 SPS 中解出的分辨率正确。
func TestParseAVCConfigRoundTrip(t *testing.T) {
	sps, pps := mustDecodeHex(t, testSPSHex), mustDecodeHex(t, testPPSHex)

	// buildAVCSequenceHeader 前 5 字节是视频 Tag 头，之后才是 AVCDecoderConfigurationRecord
	header, err := buildAVCSequenceHeader([][]byte{sps}, [][]byte{pps}, 4)
	if err != nil {
		t.Fatalf("buildAVCSequenceHeader 返回错误: %v", err)
	}

	cfg, err := ParseAVCConfig(header[5:])
	if err != nil {
		t.Fatalf("ParseAVCConfig 返回错误: %v", err)
	}

	if hex.EncodeToString(cfg.SPS) != testSPSHex {
		t.Errorf("SPS 不一致: got=%s", hex.EncodeToString(cfg.SPS))
	}
	if hex.EncodeToString(cfg.PPS) != testPPSHex {
		t.Errorf("PPS 不一致: got=%s", hex.EncodeToString(cfg.PPS))
	}
	if cfg.NALLengthSize != 4 {
		t.Errorf("NALLengthSize 期望 4, got=%d", cfg.NALLengthSize)
	}
	// 期望值来自原始 Python 实现 parse_h264_sps 对同一段 SPS 的解析结果
	if cfg.SPSInfo.Width != 960 || cfg.SPSInfo.Height != 2184 {
		t.Errorf("分辨率期望 960x2184, got=%dx%d", cfg.SPSInfo.Width, cfg.SPSInfo.Height)
	}
}

// TestParseVideoTag 校验视频 Tag 解析：关键帧标记、包类型与 CTS 符号扩展。
func TestParseVideoTag(t *testing.T) {
	// 17 01 ffffff：关键帧 + NALU 包 + CTS = -1
	tag, err := ParseVideoTag(mustDecodeHex(t, "1701ffffff0102"), 1000)
	if err != nil {
		t.Fatalf("ParseVideoTag 返回错误: %v", err)
	}

	if !tag.Keyframe || tag.PacketType != 1 {
		t.Errorf("期望关键帧且包类型为 1, got=%v/%d", tag.Keyframe, tag.PacketType)
	}
	if tag.CompositionTime != -1 || tag.DTS != 1000 || tag.PTS != 999 {
		t.Errorf("时间戳解析错误: cts=%d dts=%d pts=%d",
			tag.CompositionTime, tag.DTS, tag.PTS)
	}
}
