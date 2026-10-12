package flv

import (
	"encoding/hex"
	"testing"
)

// 两路独立编码的 SPS/PPS：main 是 1080p 竖屏，aux 是另一台编码器的低码率流，
// 两边的 sps_id / pps_id 都是 0，正好是 dual-stream 需要重编号的场景。
const (
	mainSPSHex = "67640028acd100f0044fcb35010101400000fa000030d40e8600002625a00004c4b6f8de3c600224000002"
	mainPPSHex = "68ee3cb0"
	auxSPSHex  = "674d401facd9405005bb011000000300100000030320f1832480"
	auxPPSHex  = "68ebe3cb22c0"

	// 一个 IDR slice：first_mb_in_slice=0 slice_type=7 pic_parameter_set_id=0，
	// 尾部特意带上 00 00 03 转义字节，用于验证改写后的重新转义。
	sliceHex = "65888088840000030121ffe12a"
)

// 下面这些期望值由原始 Python 实现（rtmp_server/h264_ps.py）直接生成，
// 用于保证 Go 版本的比特级改写与 Python 版本逐字节一致。
const (
	expectedMainSPSID1  = "676400284b34403c0113f2cd4040405000003e80000c3503a180000989680001312dbe378f1800890000030080"
	expectedAuxSPSID1   = "674d401f4b365014016ec044000003000400000300c83c60c920"
	expectedAuxPPSID11  = "684abe3cb22c"
	expectedSlicePPSID1 = "658840222100000300487ff84a80"

	expectedFrame      = "0000001a674d401facd9405005bb011000000300100000030320f18324800000000668ebe3cb22c00000000d65888088840000030121ffe12a"
	expectedFrameRemap = "0000001a674d401f4b365014016ec044000003000400000300c83c60c92000000006684abe3cb22c0000000e658840222100000300487ff84a80"

	expectedInbandMain = "0000002b67640028acd100f0044fcb35010101400000fa000030d40e8600002625a00004c4b6f8de3c6002240000020000000468ee3cb0"
	expectedInbandAux  = "0000001a674d401f4b365014016ec044000003000400000300c83c60c92000000006684abe3cb22c"

	expectedDualConfig = "01640028ffe2002b67640028acd100f0044fcb35010101400000fa000030d40e8600002625a00004c4b6f8de3c600224000002001a674d401f4b365014016ec044000003000400000300c83c60c92002000468ee3cb00006684abe3cb22c"

	expectedDualString   = "<DualStreamParameterSets dual=True main(sps=0,pps=0)->(0,0) aux(sps=0,pps=0)->(1,1)>"
	expectedSingleString = "<DualStreamParameterSets dual=False main(sps=0,pps=0)->(0,0) aux(sps=None,pps=None)->(1,1)>"
)

// checkHex 比较十六进制输出，不一致时直接报错。
func checkHex(t *testing.T, name string, got []byte, want string) {
	t.Helper()

	if hex.EncodeToString(got) != want {
		t.Errorf("%s 不一致\n got=%s\nwant=%s", name, hex.EncodeToString(got), want)
	}
}

// TestParameterSetIDRewrite 校验 SPS/PPS/slice 三种 NALU 的 id 改写。
func TestParameterSetIDRewrite(t *testing.T) {
	mainSPS := mustDecodeHex(t, mainSPSHex)
	auxSPS := mustDecodeHex(t, auxSPSHex)
	auxPPS := mustDecodeHex(t, auxPPSHex)
	slice := mustDecodeHex(t, sliceHex)

	// 原始编号都是 0，这正是两路码流会互相覆盖的原因
	for _, tc := range []struct {
		name string
		read func() (uint32, error)
	}{
		{"main SPS", func() (uint32, error) { return GetSPSID(mainSPS) }},
		{"aux SPS", func() (uint32, error) { return GetSPSID(auxSPS) }},
		{"slice", func() (uint32, error) { return GetSlicePPSID(slice) }},
	} {
		id, err := tc.read()
		if err != nil {
			t.Fatalf("%s 读取 id 失败: %v", tc.name, err)
		}
		if id != 0 {
			t.Errorf("%s 原始 id 期望 0, got=%d", tc.name, id)
		}
	}

	got, err := SetSPSID(mainSPS, 1)
	if err != nil {
		t.Fatalf("SetSPSID 返回错误: %v", err)
	}
	checkHex(t, "main SPS 改写为 sps_id=1", got, expectedMainSPSID1)

	got, err = SetSPSID(auxSPS, 1)
	if err != nil {
		t.Fatalf("SetSPSID 返回错误: %v", err)
	}
	checkHex(t, "aux SPS 改写为 sps_id=1", got, expectedAuxSPSID1)

	got, err = SetPPSIDs(auxPPS, 1, 1)
	if err != nil {
		t.Fatalf("SetPPSIDs 返回错误: %v", err)
	}
	checkHex(t, "aux PPS 改写为 (pps 1, sps 1)", got, expectedAuxPPSID11)

	got, err = SetSlicePPSID(slice, 1)
	if err != nil {
		t.Fatalf("SetSlicePPSID 返回错误: %v", err)
	}
	checkHex(t, "slice 改写为 pps_id=1", got, expectedSlicePPSID1)

	// 改写之后必须还能被自己读回来
	id, err := GetSlicePPSID(got)
	if err != nil {
		t.Fatalf("GetSlicePPSID 返回错误: %v", err)
	}
	if id != 1 {
		t.Errorf("改写后的 slice pps_id 期望 1, got=%d", id)
	}
}

// TestRBSPEscapeRoundTrip 校验转义字节的插入与去除。
func TestRBSPEscapeRoundTrip(t *testing.T) {
	cases := []struct{ raw, escaped string }{
		{"000001", "00000301"},
		{"000000", "00000300"},
		{"000004", "000004"}, // 第三字节 > 3，无需转义
		{"0000", "0000"},     // 结尾的 00 00 不转义
		// 插入转义后零计数器复位，所以第二组 00 00 也要再插一次
		{"01000000000102", "010000030000030102"},
	}

	for _, tc := range cases {
		raw, escaped := mustDecodeHex(t, tc.raw), mustDecodeHex(t, tc.escaped)

		checkHex(t, "EscapeRBSP("+tc.raw+")", EscapeRBSP(raw), tc.escaped)
		checkHex(t, "UnescapeRBSP("+tc.escaped+")", UnescapeRBSP(escaped), tc.raw)
	}
}

// TestRemapAVCCFrame 校验整帧重映射：帧内自带的 SPS/PPS 与 slice 一起改编号。
func TestRemapAVCCFrame(t *testing.T) {
	frame := nalusToAVCC([][]byte{
		mustDecodeHex(t, auxSPSHex),
		mustDecodeHex(t, auxPPSHex),
		mustDecodeHex(t, sliceHex),
	}, 4)
	checkHex(t, "AVCC 帧组装", frame, expectedFrame)

	// 0 -> 0 是恒等映射，应当原样返回（Python 版本同样走这条短路）
	got, err := RemapAVCCFrame(frame, map[uint32]int{0: 0}, map[uint32]int{0: 0}, 4)
	if err != nil {
		t.Fatalf("RemapAVCCFrame 返回错误: %v", err)
	}
	checkHex(t, "恒等映射", got, expectedFrame)

	// 0 -> 1：slice 的 pps_id、帧内 SPS 的 sps_id、帧内 PPS 的两个 id 都要改
	got, err = RemapAVCCFrame(frame, map[uint32]int{0: 1}, map[uint32]int{0: 1}, 4)
	if err != nil {
		t.Fatalf("RemapAVCCFrame 返回错误: %v", err)
	}
	checkHex(t, "重映射到 (1, 1)", got, expectedFrameRemap)
}

// TestDualStreamParameterSets 校验双流参数集的编号方案、sequence header 与 in-band 参数集。
func TestDualStreamParameterSets(t *testing.T) {
	mainSPS, mainPPS := mustDecodeHex(t, mainSPSHex), mustDecodeHex(t, mainPPSHex)
	auxSPS, auxPPS := mustDecodeHex(t, auxSPSHex), mustDecodeHex(t, auxPPSHex)

	ds, err := NewDualStreamParameterSets(mainSPS, mainPPS, auxSPS, auxPPS, 4, nil)
	if err != nil {
		t.Fatalf("NewDualStreamParameterSets 返回错误: %v", err)
	}

	if !ds.Dual {
		t.Fatal("两路都有 SPS/PPS 时应当处于 dual 模式")
	}
	if ds.String() != expectedDualString {
		t.Errorf("描述不一致\n got=%s\nwant=%s", ds.String(), expectedDualString)
	}

	if len(ds.SPSList) != 2 || len(ds.PPSList) != 2 {
		t.Fatalf("sequence header 应当写入两份参数集, got=%d/%d",
			len(ds.SPSList), len(ds.PPSList))
	}

	config, err := BuildAVCDecoderConfig(ds.SPSList, ds.PPSList, ds.NALLengthSize)
	if err != nil {
		t.Fatalf("BuildAVCDecoderConfig 返回错误: %v", err)
	}
	checkHex(t, "双份参数集的 AVCDecoderConfigurationRecord", config, expectedDualConfig)

	checkHex(t, "main in-band 参数集", ds.InbandParameterSets(false), expectedInbandMain)
	checkHex(t, "aux in-band 参数集", ds.InbandParameterSets(true), expectedInbandAux)

	// 单路模式：没有 aux 参数集时不写第二份，也不下发 aux in-band 参数集
	single, err := NewDualStreamParameterSets(mainSPS, mainPPS, nil, nil, 4, nil)
	if err != nil {
		t.Fatalf("NewDualStreamParameterSets 返回错误: %v", err)
	}
	if single.Dual {
		t.Error("缺少 aux 参数集时不应处于 dual 模式")
	}
	if single.String() != expectedSingleString {
		t.Errorf("描述不一致\n got=%s\nwant=%s", single.String(), expectedSingleString)
	}
	if len(single.InbandParameterSets(true)) != 0 {
		t.Error("单路模式下 aux in-band 参数集应为空")
	}

	// main 无参数集时必须报错
	if _, err := NewDualStreamParameterSets(nil, mainPPS, nil, nil, 4, nil); err == nil {
		t.Error("main 缺少 SPS 时应当返回错误")
	}
}

// TestRemapCache 校验同一帧的重映射结果会被缓存复用。
func TestRemapCache(t *testing.T) {
	ds, err := NewDualStreamParameterSets(
		mustDecodeHex(t, mainSPSHex), mustDecodeHex(t, mainPPSHex),
		mustDecodeHex(t, auxSPSHex), mustDecodeHex(t, auxPPSHex), 4, nil)
	if err != nil {
		t.Fatalf("NewDualStreamParameterSets 返回错误: %v", err)
	}

	frame := mustDecodeHex(t, expectedFrame)
	cache := NewFrameCache()

	first := ds.Remap(frame, cache, true)
	checkHex(t, "aux 帧重映射", first, expectedFrameRemap)

	// 第二次必须命中缓存，返回同一段底层数据
	if second := ds.Remap(frame, cache, true); &second[0] != &first[0] {
		t.Error("第二次重映射应当命中缓存")
	}

	// main 帧走另一个 key，结果是恒等映射
	checkHex(t, "main 帧重映射", ds.Remap(frame, cache, false), expectedFrame)
}
