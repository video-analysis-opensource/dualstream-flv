// dualstream-flv 的 Go 版本入口。
//
// 服务由两部分组成：
//
//	RTMP 服务  (0.0.0.0:1397)  接收推流，把视频帧写入内存缓冲区
//	HTTP-FLV 服务 (0.0.0.0:1398) 从缓冲区取帧，实时封装成 FLV 下发给播放器
//
// 推流地址示例：
//
//	rtmp://<ip>:1397/live/chan0            主流
//	rtmp://<ip>:1397/live/chan0-auxiliary  辅助流（低码率、GOP 更短，用于秒开）
//
// 播放地址示例：
//
//	http://<ip>:1398/play0.flv        普通模式
//	http://<ip>:1398/dual/play0.flv   双流模式（先播辅助流关键帧，实现秒开）
package main

import (
	"log"
	"os"

	"dualstream-flv/internal/server"
)

// 服务监听参数，与 Python 版本保持一致。
const (
	listenHost = "0.0.0.0"
	rtmpPort   = 1397
	flvPort    = 1398
)

func main() {
	// 日志统一输出到标准输出，并带上日期时间（对应 Python 的 logging.basicConfig）
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("[dualstream-flv] starting ...")

	// 1. 启动 RTMP 接收服务（非阻塞，后台 goroutine 接受连接）
	rtmpServer := server.NewRTMPServer(listenHost, rtmpPort, server.DefaultMaxClients)
	if err := rtmpServer.Start(); err != nil {
		log.Fatalf("[dualstream-flv] RTMP 服务启动失败: %v", err)
	}
	defer rtmpServer.Close()

	// 2. 启动 HTTP-FLV 分发服务（阻塞运行）

	flvServer := server.NewFLVServer(rtmpServer, listenHost, flvPort)
	if err := flvServer.ListenAndServe(); err != nil {
		log.Fatalf("[dualstream-flv] FLV 服务启动失败: %v", err)
	}
}
