// logutil.go 提供与 Python 版本 print_to_logger 等价的日志输出helper。

package server

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// printToLogger 把任意个参数用空格连接后输出一条日志，
// 等价于 Python 版本的 print_to_logger(*args)。
func printToLogger(args ...any) {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, fmt.Sprint(arg))
	}
	log.Printf("[rtmp.server] %s", strings.Join(parts, " "))
}

// unixSeconds 把时间格式化成「秒 + 小数」的字符串，
// 与 Python 中直接打印 time.time() 的效果一致（例如 1787736575.9465668）。
func unixSeconds(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', -1, 64)
}
