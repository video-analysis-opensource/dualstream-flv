// http_flv.go 对应 Python 版本 dualstream-flv.py 中的 flv_server_handle：
// 通过 HTTP 把缓冲区里的帧实时封装成 FLV 流下发给播放器。
//
// 路由使用 gin 框架，形如：
//
//	GET /play0.flv        普通模式：从主流的最近一个关键帧开始播放
//	GET /dual/play0.flv   双流模式：主流最新帧不是关键帧时，先用辅助流的关键帧快速起播
//
// 说明：真正下发 FLV 数据时会把连接从 net/http 中「劫持」出来，
// 直接向 TCP 连接写裸字节。这样做有两个原因：
//  1. 与 Python 版本逐字节一致（不使用 chunked 分块传输编码）；
//  2. 可以直接设置 32MB 的发送缓冲区，避免推流抖动导致卡顿。

package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"dualstream-flv/internal/flv"
	"dualstream-flv/internal/session"
	"github.com/gin-gonic/gin"
)

const (
	// maxStreamIndex 是可播放的最大通道号，与 Python 版本的正则 /play[0-9].flv 保持一致。
	maxStreamIndex = 9
	// sendBufferSize 是下发 FLV 时使用的 socket 发送缓冲区大小。
	sendBufferSize = 32 * 1024 * 1024
	// readHeaderTimeout 是读取 HTTP 请求头的超时时间，对应 Python 的 gevent.Timeout(2)。
	readHeaderTimeout = 2 * time.Second
	// noDataTimeout 是超过该时长未取到新数据即结束播放。
	noDataTimeout = 10 * time.Second
	// frameRetryTimes 是单帧的最大重试次数。
	frameRetryTimes = 50
	// frameRetryInterval 是每次重试之间的等待时间。
	frameRetryInterval = 2 * time.Millisecond
)

// httpOKHeader 是回给播放器的 HTTP 响应头，内容与 Python 版本完全一致。
var httpOKHeader = []byte(
	"HTTP/1.1 200 OK\r\n" +
		"Access-Control-Allow-Methods: GET, OPTIONS\r\n" +
		"Access-Control-Allow-Origin: *\r\n" +
		"Access-Control-Allow-Credentials: true\r\n" +
		"Content-Type: video/x-flv\r\n" +
		"\r\n")

// FLVServer 是 HTTP-FLV 分发服务。
type FLVServer struct {
	Host string
	Port int
	RTMP *RTMPServer // 帧数据来源
}

// NewFLVServer 创建 HTTP-FLV 分发服务。
func NewFLVServer(rtmpServer *RTMPServer, host string, port int) *FLVServer {
	return &FLVServer{Host: host, Port: port, RTMP: rtmpServer}
}

// Router 构建 gin 路由。
//
// 路由是用循环批量注册的：对每个通道号分别注册普通模式与双流模式两条路由，
// 处理函数由 playHandler 这个「返回处理函数的函数」生成。
func (f *FLVServer) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	// 只使用 Recovery 中间件：访问日志由业务自己按 Python 版本的格式打印。
	engine := gin.New()
	engine.Use(gin.Recovery())

	for index := 0; index <= maxStreamIndex; index++ {
		engine.GET(fmt.Sprintf("/play%d.flv", index), f.playHandler(index, false))
		engine.GET(fmt.Sprintf("/dual/play%d.flv", index), f.playHandler(index, true))
	}

	engine.NoRoute(func(c *gin.Context) {
		printToLogger("error! unknown path", c.Request.URL.Path)
		c.Status(http.StatusNotFound)
	})
	return engine
}

// ListenAndServe 启动 HTTP-FLV 服务并阻塞运行。
func (f *FLVServer) ListenAndServe() error {
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", f.Host, f.Port),
		Handler:           f.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	printToLogger("FLV listening on", fmt.Sprintf("%s:%d", f.Host, f.Port))
	return server.ListenAndServe()
}

// playHandler 生成某个通道、某种模式下的播放处理函数。
func (f *FLVServer) playHandler(streamIndex int, dualMode bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		printToLogger("connected", os.Getpid(), c.Request.RemoteAddr,
			unixSeconds(time.Now()))
		printToLogger(c.Request.Header, c.Request.URL.Path)
		printToLogger("request.path", c.Request.URL.Path)

		conn, err := hijack(c)
		if err != nil {
			printToLogger("error! hijack connection failed !", err)
			return
		}
		defer conn.Close()

		// 放大发送缓冲区，避免推流抖动时写阻塞
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.SetWriteBuffer(sendBufferSize)
		}

		if _, err := conn.Write(httpOKHeader); err != nil {
			printToLogger("error! send http header", err)
			return
		}
		printToLogger("send http header", unixSeconds(time.Now()))

		f.streamFLV(conn, streamIndex, dualMode)
		printToLogger("end.....")
	}
}

// hijack 把连接从 net/http 中接管过来，之后由业务代码自行读写。
func hijack(c *gin.Context) (net.Conn, error) {
	hijacker, ok := c.Writer.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("当前 ResponseWriter 不支持 Hijack")
	}

	conn, _, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// streamFLV 是下发 FLV 流的核心循环。
//
// 处理流程：
//  1. 取出主流的 SPS/PPS/宽高，把 main 与 aux 两路参数集错开编号后
//     一起写进 FLV 头（含 onMetaData 与 AVC 序列头）；
//  2. 选定起播位置：普通模式从主流最近的关键帧开始；
//     双流模式下若主流最新帧不是关键帧，则先播放辅助流（低码率、GOP 短）的帧，
//     直到主流出现关键帧后再切回主流，从而实现秒开；
//  3. 逐帧从缓冲区取数据，按来源改写 slice header 里的 pic_parameter_set_id，
//     再封装成 FLV Tag 写给播放器。
func (f *FLVServer) streamFLV(w io.Writer, streamIndex int, dualMode bool) {
	mainKey := fmt.Sprintf("chan%d", streamIndex)
	auxiliaryKey := fmt.Sprintf("chan%d-auxiliary", streamIndex)

	mainBuffer, auxBuffer := f.RTMP.Buffer(mainKey), f.RTMP.Buffer(auxiliaryKey)
	if mainBuffer == nil || auxBuffer == nil {
		printToLogger("error! unknown stream key", mainKey)
		return
	}

	// ---------- 1. 解码参数 ----------
	meta := mainBuffer.Meta()
	if len(meta.SPS) == 0 || len(meta.PPS) == 0 {
		printToLogger("error! sps/pps is empty, stream not ready:", mainKey)
		return
	}
	printToLogger("pps len", len(meta.PPS), "sps len", len(meta.SPS),
		"width", meta.Width, "height", meta.Height)
	printToLogger("time_base", "1/1000")

	// main 和 aux 是两路独立编码的码流，各自有一份 sps/pps，
	// 而且两边一般都用 sps_id=0 / pps_id=0。只写其中一份，
	// 另一路的帧就解不出来。
	// 这里把 main 的参数集重编号成 (sps 0, pps 0)、aux 重编号成
	// (sps 1, pps 1)，两份一起写进 AVC sequence header，
	// 再在发送每一帧时改写 slice header 里的 pic_parameter_set_id，
	// 指定这一帧该用哪一份 sps/pps。
	auxMeta := auxBuffer.Meta()

	var auxSPS, auxPPS []byte
	if dualMode {
		auxSPS, auxPPS = auxMeta.SPS, auxMeta.PPS
	}

	dualPS, err := flv.NewDualStreamParameterSets(meta.SPS, meta.PPS,
		auxSPS, auxPPS, meta.NALLengthSize, printToLogger)
	if err != nil {
		printToLogger("error! build dual parameter sets", err)
		return
	}
	printToLogger("dual parameter sets", dualPS)

	if dualMode && !dualPS.Dual {
		printToLogger("warning! aux stream has no sps/pps yet," +
			" aux frames will not be decodable")
	}
	if dualMode && dualPS.Dual &&
		(auxMeta.Width != meta.Width || auxMeta.Height != meta.Height) {
		printToLogger("warning! main/aux resolution mismatch",
			fmt.Sprintf("(%d, %d)", meta.Width, meta.Height),
			fmt.Sprintf("(%d, %d)", auxMeta.Width, auxMeta.Height))
	}

	// ---------- 2. 选定起播位置 ----------
	mainLastFrame, err := mainBuffer.LastItem()
	if err != nil {
		printToLogger(err)
		return
	}

	useAux, currentVCounter := choosePlayStart(dualMode, mainLastFrame, mainBuffer, auxBuffer)

	// ---------- 3. 下发 FLV 头 ----------
	initObj := flv.NewFlv(meta.Width, meta.Height)

	initData, err := initObj.GetInitFLV(dualPS.SPSList, dualPS.PPSList, dualPS.NALLengthSize)
	if err != nil {
		printToLogger("error! get_init_flv", err)
	} else if _, err := w.Write(initData); err != nil {
		printToLogger("error! get_init_flv", err)
	}

	// ---------- 4. 逐帧下发 ----------
	lastDataTime := time.Now()
	printToLogger("last_data_time", unixSeconds(lastDataTime))

	findFirstKey := false // 是否已经遇到主流的第一个关键帧
	// 切换到某一路时，在它的第一个关键帧前再带一次该路的 sps/pps。
	// sequence header 里已经有两份了，这里重复一次是为了保证
	// 部分只认 in-band 参数集的播放器/硬解也能正确激活。
	auxPSSent, mainPSSent := false, false

	var auxFrame *session.VideoFrame // 本轮使用的辅助流帧（未启用辅助流时为 nil）
	var mainFrame *session.VideoFrame

	for {
		// 超过 10s 没有取到新数据就结束
		if time.Since(lastDataTime) > noDataTimeout {
			printToLogger("last data time > 10 s  end .....", unixSeconds(lastDataTime))
			break
		}

// 		printToLogger("try get current_v_counter", currentVCounter)

		// 取帧：最多重试 50 次，每次间隔 2ms。
		fetched := fetchFrames(fetchParams{
			mainBuffer:     mainBuffer,
			auxBuffer:      auxBuffer,
			counter:        currentVCounter,
			useAux:         useAux,
			lastDataTime:   lastDataTime,
			currentAuxHint: auxFrame,
		})

		if !fetched.ok {
			// 没取到新帧，不要把上一帧重复发一遍，
			// 也不要刷新 lastDataTime，否则 10s 超时永远不会触发。
			// 辅助流帧可能已经被更新（与 Python 的变量残留行为一致）
			auxFrame = fetched.aux
			continue
		}

		auxFrame, mainFrame = fetched.aux, fetched.main
		useAux = fetched.useAux
		currentVCounter++

		// ---------- 核心处理流程 ----------
		lastDataTime = time.Now()

		// aux 帧的 slice header 指向 pps_id=1(aux 那份)，
		// main 帧指向 pps_id=0(main 那份)；结果按帧缓存，
		// 多个播放连接共用，比特改写只做一次。
		dataFrameI := remapFrame(dualPS, auxFrame, true)
		dataFrameIIsKey := auxFrame != nil && auxFrame.IsKeyframe
		dataFrameRaw := remapFrame(dualPS, mainFrame, false)
		dataFrameRawIsKey := mainFrame.IsKeyframe
		framePTS := uint32(mainFrame.PTS)

		// 主流出现第一个关键帧之前先播辅助流，之后固定播主流
		if !findFirstKey && dataFrameRawIsKey {
			findFirstKey = true
		}

		// 未遇到主流关键帧时下发辅助流，之后固定下发主流；
		// 某一路第一次真正下发数据时，在帧前补一次该路的 in-band 参数集。
		payload, isKey := dataFrameI, dataFrameIIsKey
		if findFirstKey {
			payload, isKey = dataFrameRaw, dataFrameRawIsKey

			if len(payload) > 0 && !mainPSSent {
				payload = prependParameterSets(dualPS.InbandParameterSets(false), payload)
				mainPSSent = true
			}
		} else if len(payload) > 0 && !auxPSSent {
			payload = prependParameterSets(dualPS.InbandParameterSets(true), payload)
			auxPSSent = true
		}

		if _, err := w.Write(initObj.GetVideo(framePTS, payload, isKey)); err != nil {
			printToLogger("sock.sendall error", err)
			break
		}
	}
}

// remapFrame 按帧的来源（main / aux）改写其中 slice header 的 pic_parameter_set_id。
//
// 帧为 nil 时返回空数据，对应 Python 中 remap(None) 返回 b""。
func remapFrame(ps *flv.DualStreamParameterSets, frame *session.VideoFrame, aux bool) []byte {
	if frame == nil {
		return nil
	}
	return ps.Remap(frame.AVCCData, frame.RemapCache, aux)
}

// prependParameterSets 把 in-band 参数集拼在帧数据前面。
//
// 帧数据可能是多个连接共享的缓存，这里必须拷贝出新的切片，不能就地修改。
func prependParameterSets(parameterSets, frameData []byte) []byte {
	if len(parameterSets) == 0 {
		return frameData
	}

	out := make([]byte, 0, len(parameterSets)+len(frameData))
	out = append(out, parameterSets...)
	return append(out, frameData...)
}

// choosePlayStart 计算起播位置，返回 (是否使用辅助流, 起播的帧序号)。
//
//   - 普通模式：始终从主流最近的关键帧开始；
//   - 双流模式：主流最新帧已经是关键帧时同上；否则改用辅助流最近的关键帧，
//     等待主流下一个关键帧到来后再切回主流。
func choosePlayStart(dualMode bool, mainLastFrame session.VideoFrame,
	mainBuffer, auxBuffer *Buffer) (useAux bool, counter int) {

	if dualMode && !mainLastFrame.IsKeyframe {
		counter = auxBuffer.LastKeyframeCursor()
		printToLogger("DUALMODE [main_last_frame]", mainLastFrame.PTS,
			"is not keyframe, use aux keyframe", counter)
		return true, counter
	}

	counter = mainBuffer.LastKeyframeCursor()
	if dualMode {
		printToLogger("DUALMODE main_last_frame", mainLastFrame.PTS,
			"is keyframe, use it!!!", counter)
	} else {
		printToLogger("OLDMODE main_last_frame", mainLastFrame.PTS,
			"is keyframe, use it!!!", counter)
	}
	return false, counter
}

// fetchParams 是 fetchFrames 的入参。
type fetchParams struct {
	mainBuffer     *Buffer
	auxBuffer      *Buffer
	counter        int                 // 要取的帧序号
	useAux         bool                // 当前是否处于辅助流模式
	lastDataTime   time.Time           // 仅用于打印日志，与 Python 版本对齐
	currentAuxHint *session.VideoFrame // 上一轮的辅助流帧，取帧失败时原样返回
}

// fetchResult 是 fetchFrames 的返回结果。
type fetchResult struct {
	ok     bool                // 是否成功取到主流帧
	main   *session.VideoFrame // 主流帧
	aux    *session.VideoFrame // 辅助流帧（未启用辅助流时为 nil）
	useAux bool                // 取帧后是否仍处于辅助流模式
}

// fetchFrames 尝试取出指定序号的帧，最多重试 50 次，每次间隔 2ms。
//
// 一旦取到的主流帧是关键帧，就说明主流已经可以独立解码，辅助流模式自动关闭。
func fetchFrames(p fetchParams) fetchResult {
	result := fetchResult{aux: p.currentAuxHint, useAux: p.useAux}
// 	timestamp := unixSeconds(p.lastDataTime)

	for i := 0; i < frameRetryTimes; i++ {
		if result.useAux {
			auxFrame, ok := p.auxBuffer.Get(p.counter)
			if !ok {
// 				printToLogger("error! except", fmt.Sprintf("aux frame %d not found", p.counter))
				time.Sleep(frameRetryInterval)
				continue
			}
			result.aux = &auxFrame
// 			printToLogger("AUX BUFFER", timestamp, p.counter,
// 				len(auxFrame.AVCCData), auxFrame.IsKeyframe)
		} else {
			result.aux = nil
// 			printToLogger("AUX BUFFER", timestamp, p.counter, "NONE.....")
		}

		mainFrame, ok := p.mainBuffer.Get(p.counter)
		if !ok {
// 			printToLogger("error! except", fmt.Sprintf("main frame %d not found", p.counter))
			time.Sleep(frameRetryInterval)
			continue
		}
// 		printToLogger("MAIN BUFFER", timestamp, p.counter,
// 			len(mainFrame.AVCCData), mainFrame.IsKeyframe)

		if mainFrame.IsKeyframe {
			result.useAux = false
		}

		result.main = &mainFrame
		result.ok = true
		return result
	}
	return result
}
