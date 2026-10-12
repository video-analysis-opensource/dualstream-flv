# dualstream-flv（Go 版本）

本项目是 `rtmp_sample_code/dualstream-flv.py` 的 Go 语言实现，功能与 Python 版本保持一致：

- 接收 RTMP 推流，把视频帧缓存在内存中（按 GOP 自动淘汰旧帧）；
- 通过 HTTP-FLV 把缓存中的帧实时下发给播放器；
- 支持「双流秒开」：主流最新帧不是关键帧时，先播放辅助流的关键帧，
  等主流关键帧到来后再无缝切回主流；
- 双流参数集重编号：main 与 aux 是两路独立编码的码流，各自的 sps/pps
  通常都用 id 0，直接混在一路 FLV 里会互相覆盖。这里把 main 重编号成
  (sps 0, pps 0)、aux 重编号成 (sps 1, pps 1)，两份一起写进 AVC 序列头，
  再逐帧改写 slice header 里的 `pic_parameter_set_id` 指定该用哪一份。

## 目录结构

```
main.go                     程序入口：启动 RTMP(1397) 与 HTTP-FLV(1398) 两个服务
internal/amf0/              AMF0 编解码            <- rtmp_server/amf0.py
internal/rtmp/              握手、Chunk 分块、消息封装 <- rtmp_server/rtmp.py
internal/flv/parse.go       FLV Tag / AVC 配置 / SPS 解析 <- rtmp_server/flv.py
internal/flv/writer.go      FLV 头与视频 Tag 生成    <- rtmp_server/init_flv.py
internal/flv/h264_ps.go     SPS/PPS 重编号与帧重映射 <- rtmp_server/h264_ps.py
internal/session/           单条 RTMP 推流会话      <- rtmp_server/session.py
internal/server/buffer.go   帧缓冲区 RTMPBuffer     <- dualstream-flv.py
internal/server/rtmp_server.go  RTMP 接收服务       <- dualstream-flv.py
internal/server/http_flv.go     HTTP-FLV 分发（gin） <- dualstream-flv.py
```

## 运行

```bash
go run .
```

推流地址：

```
rtmp://<ip>:1397/live/chan0            主流
rtmp://<ip>:1397/live/chan0-auxiliary  辅助流
```

可用的流名为 `chan0` ~ `chan10` 及各自的 `-auxiliary` 辅助流。

播放地址：

```
http://<ip>:1398/play0.flv        普通模式：从主流最近的关键帧开始播放
http://<ip>:1398/dual/play0.flv   双流模式：先播辅助流关键帧实现秒开
```

播放路径的通道号范围是 0~9，与 Python 版本的正则 `^(/dual)?/play[0-9].flv$` 一致。

## 与 Python 版本的对应关系

- **输出字节完全一致**：FLV 文件头、onMetaData、AVC 序列头、视频 Tag、
  AMF0 命令、RTMP 消息封装的字节序列均与 Python 版本逐字节相同，
  相关单元测试中的期望值都是直接用原始 Python 代码生成的。
- **HTTP 响应头一致**：下发 FLV 时会把连接从 `net/http` 中劫持出来直接写裸字节，
  因此响应头与 Python 版本完全相同，也不会使用 chunked 分块传输编码。
- **并发模型**：Python 使用 gevent 协程池（`Pool(1024)`），
  Go 版本用带缓冲的 channel 作为信号量实现同样的并发上限；
  帧缓冲区改为读写锁保护，以适配 Go 的真并发。
- **参数集重映射逐字节一致**：`internal/flv/h264_ps.go` 的比特级改写
  （SPS/PPS/slice 的 id 改写、转义字节的插入与去除、AVCDecoderConfigurationRecord
  的生成）与 `rtmp_server/h264_ps.py` 输出完全相同，单元测试的期望值同样
  由原始 Python 代码生成。改写结果按帧缓存（`flv.FrameCache` 对应 Python 的
  `VideoFrame.remap_cache`），多个播放连接共用同一帧时只做一次。
- **未移植的部分**：`setproctitle`（Go 无对应能力）、
  `flv.py` 中把流录制成本地文件的 `FLVWriter`（原程序并未使用）。

## 测试

```bash
go test ./...
```
