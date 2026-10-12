package amf0

import (
	"encoding/hex"
	"testing"
)

// 下列期望值均由原始 Python 实现 rtmp_server/amf0.py 的 encode_command 生成，
// 用于确保 Go 版本编码出的字节与 Python 版本完全一致。
const (
	expectedConnectResultHex = "0200075f726573756c74003ff000000000000003" +
		"0006666d7356657202000e464d532f332c352c372c37303039000c636170616269" +
		"6c697469657300403f0000000000000000090300056c6576656c02000673746174" +
		"75730004636f646502001d4e6574436f6e6e656374696f6e2e436f6e6e6563742e" +
		"53756363657373000b6465736372697074696f6e020015436f6e6e656374696f6e" +
		"207375636365656465642e000e6f626a656374456e636f64696e67000000000000" +
		"000000000009"

	expectedCreateStreamHex = "0200075f726573756c740040100000000000000500" +
		"3ff0000000000000"

	expectedOnStatusHex = "0200086f6e537461747573000000000000000000050300" +
		"056c6576656c0200067374617475730004636f64650200174e657453747265616d" +
		"2e5075626c6973682e5374617274000b6465736372697074696f6e020010507562" +
		"6c697368696e67206368616e30000009"

	connectCommandHex = "020007636f6e6e656374003ff000000000000003000361707002" +
		"00046c6976650005746355726c02001a72746d703a2f2f3132372e302e302e313a" +
		"313339372f6c697665000009"
)

// TestEncodeCommand 校验三条常用命令的编码结果与 Python 版本一致。
func TestEncodeCommand(t *testing.T) {
	cases := []struct {
		name    string
		command string
		tid     float64
		args    []any
		want    string
	}{
		{
			name:    "connect 的 _result",
			command: "_result",
			tid:     1.0,
			args: []any{
				NewObject("fmsVer", "FMS/3,5,7,7009", "capabilities", 31.0),
				NewObject(
					"level", "status",
					"code", "NetConnection.Connect.Success",
					"description", "Connection succeeded.",
					"objectEncoding", 0.0,
				),
			},
			want: expectedConnectResultHex,
		},
		{
			name:    "createStream 的 _result",
			command: "_result",
			tid:     4.0,
			args:    []any{nil, 1.0},
			want:    expectedCreateStreamHex,
		},
		{
			name:    "publish 的 onStatus",
			command: "onStatus",
			tid:     0,
			args: []any{nil, NewObject(
				"level", "status",
				"code", "NetStream.Publish.Start",
				"description", "Publishing chan0",
			)},
			want: expectedOnStatusHex,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EncodeCommand(tc.command, tc.tid, tc.args...)
			if err != nil {
				t.Fatalf("EncodeCommand 返回错误: %v", err)
			}

			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("编码结果不一致\n got=%s\nwant=%s", hex.EncodeToString(got), tc.want)
			}
		})
	}
}

// TestDecodeAll 校验解码一条真实的 connect 命令。
func TestDecodeAll(t *testing.T) {
	raw, err := hex.DecodeString(connectCommandHex)
	if err != nil {
		t.Fatalf("非法的测试数据: %v", err)
	}

	values, err := DecodeAll(raw)
	if err != nil {
		t.Fatalf("DecodeAll 返回错误: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("期望解码出 3 个值, got=%d", len(values))
	}

	if name, _ := values[0].(string); name != "connect" {
		t.Errorf("命令名期望 connect, got=%v", values[0])
	}
	if tid, _ := values[1].(float64); tid != 1.0 {
		t.Errorf("事务 ID 期望 1.0, got=%v", values[1])
	}

	props, ok := values[2].(map[string]any)
	if !ok {
		t.Fatalf("第三个值期望是对象, got=%T", values[2])
	}
	if props["app"] != "live" {
		t.Errorf("app 期望 live, got=%v", props["app"])
	}
	if props["tcUrl"] != "rtmp://127.0.0.1:1397/live" {
		t.Errorf("tcUrl 不正确, got=%v", props["tcUrl"])
	}
}

// TestDecodeEncodeRoundTrip 校验编码后再解码可以还原出等价的值。
func TestDecodeEncodeRoundTrip(t *testing.T) {
	encoded, err := EncodeCommand("publish", 3.0, nil, "chan0-auxiliary", "live")
	if err != nil {
		t.Fatalf("EncodeCommand 返回错误: %v", err)
	}

	values, err := DecodeAll(encoded)
	if err != nil {
		t.Fatalf("DecodeAll 返回错误: %v", err)
	}

	want := []any{"publish", 3.0, nil, "chan0-auxiliary", "live"}
	if len(values) != len(want) {
		t.Fatalf("期望 %d 个值, got=%d", len(want), len(values))
	}
	for i := range want {
		if values[i] != want[i] {
			t.Errorf("第 %d 个值期望 %v, got=%v", i, want[i], values[i])
		}
	}
}
