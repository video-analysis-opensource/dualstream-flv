package flv

import "testing"

// TestParseH264SPS 用不同 profile 的 SPS 校验解析结果。
//
// 所有期望值均由原始 Python 实现 rtmp_server/flv.py 的 parse_h264_sps
// 对同一段数据计算得到，用于确保 Go 版本与 Python 版本行为完全一致。
func TestParseH264SPS(t *testing.T) {
	cases := []struct {
		name       string
		spsHex     string
		profileIDC uint32
		width      int
		height     int
		cropTop    uint32
		cropBottom uint32
	}{
		{
			name:       "High profile 带裁剪",
			spsHex:     testSPSHex,
			profileIDC: 100,
			width:      960,
			height:     2184,
			cropTop:    4,
		},
		{
			name:       "Main profile 1280x720",
			spsHex:     "674d401f9a6602802dd80880000003008000001e478c195001e478c19500",
			profileIDC: 77,
			width:      1280,
			height:     720,
		},
		{
			name:       "Baseline profile 1920x1080",
			spsHex:     "6742c01fda01e0089f9611000003000100000300321e2c5c90",
			profileIDC: 66,
			width:      1920,
			height:     1080,
			cropBottom: 4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := ParseH264SPS(mustDecodeHex(t, tc.spsHex))
			if err != nil {
				t.Fatalf("ParseH264SPS 返回错误: %v", err)
			}

			if info.ProfileIDC != tc.profileIDC {
				t.Errorf("profile_idc 期望 %d, got=%d", tc.profileIDC, info.ProfileIDC)
			}
			if info.Width != tc.width || info.Height != tc.height {
				t.Errorf("分辨率期望 %dx%d, got=%dx%d",
					tc.width, tc.height, info.Width, info.Height)
			}
			if info.CropTop != tc.cropTop || info.CropBottom != tc.cropBottom {
				t.Errorf("裁剪参数期望 top=%d bottom=%d, got top=%d bottom=%d",
					tc.cropTop, tc.cropBottom, info.CropTop, info.CropBottom)
			}
		})
	}
}

// TestParseAVCNALUs 校验 AVCC 拆包，以及与 NALUsToAVCC 的互逆关系。
func TestParseAVCNALUs(t *testing.T) {
	// 两个 NALU：长度 3 的 "65 88 84" 与长度 2 的 "41 9a"
	payload := mustDecodeHex(t, "0000000365888400000002419a")

	nalus, err := ParseAVCNALUs(payload, 4)
	if err != nil {
		t.Fatalf("ParseAVCNALUs 返回错误: %v", err)
	}
	if len(nalus) != 2 {
		t.Fatalf("期望解析出 2 个 NALU, got=%d", len(nalus))
	}

	rebuilt, err := NALUsToAVCC(nalus, 4)
	if err != nil {
		t.Fatalf("NALUsToAVCC 返回错误: %v", err)
	}
	if string(rebuilt) != string(payload) {
		t.Errorf("AVCC 重新组包结果与原始数据不一致")
	}
}

// TestParseAVCNALUsTruncated 校验被截断的数据会返回错误（与 Python 版本抛异常对应）。
func TestParseAVCNALUsTruncated(t *testing.T) {
	if _, err := ParseAVCNALUs(mustDecodeHex(t, "0000000a6588"), 4); err == nil {
		t.Fatal("数据被截断时期望返回错误")
	}
}
