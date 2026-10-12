// Package amf0 实现 RTMP 控制消息使用的 AMF0 编解码。
//
// 该包是对 Python 版本 rtmp_server/amf0.py 的等价实现：
//   - 解码：decode_one / decode_all
//   - 编码：encode_value / encode_command
//
// 设计上全部采用「纯函数」风格：解码函数接受 (数据, 游标) 返回
// (值, 新游标, 错误)，不持有任何内部状态；编码函数接受值返回新的字节切片，
// 不会修改入参。这样这些函数天然是并发安全且可组合的。
package amf0

import (
	"encoding/binary"
	"fmt"
	"math"
)

// AMF0 类型标记（marker）。
const (
	markerNumber     = 0x00
	markerBoolean    = 0x01
	markerString     = 0x02
	markerObject     = 0x03
	markerNull       = 0x05
	markerUndefined  = 0x06
	markerECMAArray  = 0x08
	markerObjectEnd  = 0x09
	markerStrictArr  = 0x0A
	markerDate       = 0x0B
	markerLongString = 0x0C
)

// Field 表示一个「有序对象」中的一个键值对。
//
// Go 的 map 遍历顺序是随机的，而 Python 的 dict 会保持插入顺序，
// 因此编码对象时使用有序的 Object 结构，保证生成的字节序列与 Python 版本一致。
type Field struct {
	Key   string
	Value any
}

// Object 是一个保持插入顺序的 AMF0 对象。
type Object []Field

// NewObject 由若干键值对构造一个有序对象，调用形式接近字面量：
//
//	amf0.NewObject("level", "status", "code", "NetStream.Publish.Start")
//
// 参数个数必须为偶数，且奇数位必须是 string，否则该键值对被忽略。
func NewObject(kv ...any) Object {
	obj := make(Object, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if key, ok := kv[i].(string); ok {
			obj = append(obj, Field{Key: key, Value: kv[i+1]})
		}
	}
	return obj
}

// readU16 读取一个大端 uint16，返回值与新游标。
func readU16(data []byte, p int) (uint16, int, error) {
	if p+2 > len(data) {
		return 0, p, fmt.Errorf("amf0: 数据不足，无法读取 uint16")
	}
	return binary.BigEndian.Uint16(data[p : p+2]), p + 2, nil
}

// readU32 读取一个大端 uint32，返回值与新游标。
func readU32(data []byte, p int) (uint32, int, error) {
	if p+4 > len(data) {
		return 0, p, fmt.Errorf("amf0: 数据不足，无法读取 uint32")
	}
	return binary.BigEndian.Uint32(data[p : p+4]), p + 4, nil
}

// readBytes 读取 n 字节，返回切片与新游标。
func readBytes(data []byte, p, n int) ([]byte, int, error) {
	if p+n > len(data) {
		return nil, p, fmt.Errorf("amf0: 数据不足，需要 %d 字节", n)
	}
	return data[p : p+n], p + n, nil
}

// DecodeOne 从 data[p] 处解码一个 AMF0 值，返回 (值, 新游标, 错误)。
//
// 值到 Go 类型的映射关系：
//
//	number       -> float64
//	boolean      -> bool
//	string       -> string
//	null/undef   -> nil
//	object/ecma  -> map[string]any
//	strict array -> []any
//	date         -> map[string]any{"milliseconds":..., "timezone":...}
func DecodeOne(data []byte, p int) (any, int, error) {
	if p >= len(data) {
		return nil, p, fmt.Errorf("amf0: 已到数据末尾")
	}
	marker := data[p]
	p++

	switch marker {
	case markerNumber:
		raw, next, err := readBytes(data, p, 8)
		if err != nil {
			return nil, p, err
		}
		return math.Float64frombits(binary.BigEndian.Uint64(raw)), next, nil

	case markerBoolean:
		raw, next, err := readBytes(data, p, 1)
		if err != nil {
			return nil, p, err
		}
		return raw[0] != 0, next, nil

	case markerString:
		return decodeString(data, p, 16)

	case markerLongString:
		return decodeString(data, p, 32)

	case markerNull, markerUndefined:
		return nil, p, nil

	case markerObject, markerECMAArray:
		return decodeObject(data, p, marker == markerECMAArray)

	case markerStrictArr:
		return decodeStrictArray(data, p)

	case markerDate:
		raw, next, err := readBytes(data, p, 10)
		if err != nil {
			return nil, p, err
		}
		return map[string]any{
			"milliseconds": math.Float64frombits(binary.BigEndian.Uint64(raw[0:8])),
			"timezone":     int16(binary.BigEndian.Uint16(raw[8:10])),
		}, next, nil
	}

	return nil, p, fmt.Errorf("amf0: 不支持的类型标记 0x%02x", marker)
}

// decodeString 解码字符串，lenBits 为长度字段的位数（16 或 32）。
func decodeString(data []byte, p int, lenBits int) (any, int, error) {
	var n int
	var err error

	if lenBits == 16 {
		var v16 uint16
		v16, p, err = readU16(data, p)
		n = int(v16)
	} else {
		var v32 uint32
		v32, p, err = readU32(data, p)
		n = int(v32)
	}
	if err != nil {
		return nil, p, err
	}

	raw, next, err := readBytes(data, p, n)
	if err != nil {
		return nil, p, err
	}
	return string(raw), next, nil
}

// decodeObject 解码 object / ecma-array，二者结构一致，
// 仅 ecma-array 多出一个 4 字节的「关联元素个数」（该值并不可靠，只做跳过处理）。
func decodeObject(data []byte, p int, isECMAArray bool) (any, int, error) {
	var err error
	if isECMAArray {
		if _, p, err = readU32(data, p); err != nil {
			return nil, p, err
		}
	}

	obj := map[string]any{}
	for p+3 <= len(data) {
		var n uint16
		if n, p, err = readU16(data, p); err != nil {
			return nil, p, err
		}

		// 键长度为 0 且紧跟 object-end 标记，表示对象结束。
		if n == 0 && p < len(data) && data[p] == markerObjectEnd {
			return obj, p + 1, nil
		}

		keyRaw, next, err := readBytes(data, p, int(n))
		if err != nil {
			return nil, p, err
		}
		p = next

		value, next, err := DecodeOne(data, p)
		if err != nil {
			return nil, p, err
		}
		p = next
		obj[string(keyRaw)] = value
	}
	return obj, p, nil
}

// decodeStrictArray 解码严格数组。
func decodeStrictArray(data []byte, p int) (any, int, error) {
	count, p, err := readU32(data, p)
	if err != nil {
		return nil, p, err
	}

	arr := make([]any, 0, count)
	for i := uint32(0); i < count; i++ {
		var value any
		if value, p, err = DecodeOne(data, p); err != nil {
			return nil, p, err
		}
		arr = append(arr, value)
	}
	return arr, p, nil
}

// DecodeAll 解码整段数据中的所有 AMF0 值。
//
// 与 Python 版本一致：一旦遇到无法识别的数据即返回错误。
func DecodeAll(data []byte) ([]any, error) {
	out := make([]any, 0, 4)
	for p := 0; p < len(data); {
		value, next, err := DecodeOne(data, p)
		if err != nil {
			return out, err
		}
		// 防御性检查：游标必须前进，避免构造出的畸形数据导致死循环。
		if next <= p {
			return out, fmt.Errorf("amf0: 解码游标未前进，数据可能已损坏")
		}
		out = append(out, value)
		p = next
	}
	return out, nil
}

// EncodeValue 把一个 Go 值编码成 AMF0 字节序列。
//
// 支持的类型与 Python 版本的 encode_value 一一对应；不支持的类型返回错误。
func EncodeValue(v any) ([]byte, error) {
	switch value := v.(type) {
	case nil:
		return []byte{markerNull}, nil

	case bool:
		flag := byte(0)
		if value {
			flag = 1
		}
		return []byte{markerBoolean, flag}, nil

	case int:
		return encodeNumber(float64(value)), nil
	case int64:
		return encodeNumber(float64(value)), nil
	case float64:
		return encodeNumber(value), nil

	case string:
		return encodeString(value), nil

	case Object:
		return encodeObject(value)

	case []any:
		out := append([]byte{markerStrictArr}, be32(uint32(len(value)))...)
		for _, item := range value {
			encoded, err := EncodeValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, encoded...)
		}
		return out, nil
	}

	return nil, fmt.Errorf("amf0: 不支持编码的类型 %T", v)
}

// encodeNumber 编码 AMF0 number（IEEE754 双精度，大端）。
func encodeNumber(v float64) []byte {
	out := make([]byte, 9)
	out[0] = markerNumber
	binary.BigEndian.PutUint64(out[1:], math.Float64bits(v))
	return out
}

// encodeString 编码 AMF0 短字符串。
func encodeString(v string) []byte {
	out := make([]byte, 0, 3+len(v))
	out = append(out, markerString)
	out = append(out, be16(uint16(len(v)))...)
	return append(out, v...)
}

// encodeObject 编码有序对象，以 00 00 09（object-end）结尾。
func encodeObject(obj Object) ([]byte, error) {
	out := []byte{markerObject}
	for _, field := range obj {
		out = append(out, be16(uint16(len(field.Key)))...)
		out = append(out, field.Key...)

		encoded, err := EncodeValue(field.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
	}
	return append(out, 0x00, 0x00, markerObjectEnd), nil
}

// EncodeCommand 编码一条 AMF0 命令消息：命令名 + 事务 ID + 若干参数。
func EncodeCommand(name string, transactionID float64, args ...any) ([]byte, error) {
	values := append([]any{name, transactionID}, args...)

	out := make([]byte, 0, 64)
	for _, value := range values {
		encoded, err := EncodeValue(value)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// be16 返回大端 uint16 字节序列。
func be16(v uint16) []byte {
	return []byte{byte(v >> 8), byte(v)}
}

// be32 返回大端 uint32 字节序列。
func be32(v uint32) []byte {
	return []byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}
