// errno.go 单独抽出与操作系统相关的错误码，便于 isDisconnect 判断连接断开。

package server

import "syscall"

// 这两个错误码分别表示「对端已关闭写端」和「连接被对端重置」，
// 在网络编程中都属于客户端正常离开的情形。
var (
	syscallEPIPE      = syscall.EPIPE
	syscallECONNRESET = syscall.ECONNRESET
)
