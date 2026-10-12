// rtmp_server.go 对应 Python 版本 dualstream-flv.py 中的 RTMPServer：
// 监听 RTMP 端口，把每一路推流的视频帧写入对应的全局缓冲区。

package server

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"

	"dualstream-flv/internal/session"
)

// DefaultMaxClients 是默认允许的最大并发推流连接数。
const DefaultMaxClients = 1024

// RTMPServer 是 RTMP 接收服务。
//
// GlobalBuffer 中的键在创建时一次性初始化好且之后不再增删，
// 因此可以被多个 goroutine 无锁并发读取（每个 Buffer 内部各自加锁）。
type RTMPServer struct {
	Host string
	Port int

	GlobalBuffer map[string]*Buffer

	pool     chan struct{} // 并发连接数信号量，等价于 gevent 的 Pool
	listener net.Listener
}

// BufferKeys 返回全部预置的流名：chan0 ~ chan10 及其对应的 -auxiliary 辅助流。
//
// 只有推到这些流名上的数据才会被缓存，其余流名直接丢弃。
func BufferKeys() []string {
	const channelCount = 11 // chan0 ~ chan10

	keys := make([]string, 0, channelCount*2)
	for i := 0; i < channelCount; i++ {
		keys = append(keys,
			fmt.Sprintf("chan%d", i),
			fmt.Sprintf("chan%d-auxiliary", i),
		)
	}
	return keys
}

// NewRTMPServer 创建 RTMP 服务，并为每一个预置流名建好缓冲区。
func NewRTMPServer(host string, port int, maxClients int) *RTMPServer {
	if maxClients <= 0 {
		maxClients = DefaultMaxClients
	}

	buffers := make(map[string]*Buffer, len(BufferKeys()))
	for _, key := range BufferKeys() {
		buffers[key] = NewBuffer()
	}

	return &RTMPServer{
		Host:         host,
		Port:         port,
		GlobalBuffer: buffers,
		pool:         make(chan struct{}, maxClients),
	}
}

// Buffer 按流名取缓冲区，流名未预置时返回 nil。
func (s *RTMPServer) Buffer(key string) *Buffer {
	return s.GlobalBuffer[key]
}

// onVideo 返回写入缓冲区的视频帧回调。
//
// 采用「返回闭包」的方式，把 RTMPServer 捕获进回调中，
// 使 session 包完全不需要知道缓冲区的存在。
func (s *RTMPServer) onVideo() session.VideoHandler {
	return func(frame session.VideoFrame, sess *session.Session) {
		buf := s.Buffer(sess.StreamKey)
		if buf == nil {
			// 未预置的流名直接忽略
			return
		}

		buf.Append(frame, Meta{
			PPS:           sess.PPS,
			SPS:           sess.SPS,
			Width:         sess.Width,
			Height:        sess.Height,
			NALLengthSize: sess.NALLengthSize,
		})
// 		log.Printf("[rtmp.server] %s", buf)
	}
}

// handleConn 处理一条 RTMP 连接，函数返回时连接一定被关闭。
func (s *RTMPServer) handleConn(conn net.Conn) {
	addr := conn.RemoteAddr().String()

	sess := session.New(conn, session.WithVideoHandler(s.onVideo()))
	defer func() {
		_ = sess.Close()
		_ = conn.Close()
		delete(s.GlobalBuffer, sess.StreamKey)
		s.GlobalBuffer[sess.StreamKey] = NewBuffer()
	}()

	log.Printf("[rtmp.server] connection %s", addr)

	switch err := sess.Run(); {
	case err == nil:
		log.Printf("[rtmp.server] session finished %s", addr)
	case isDisconnect(err):
		// 对端正常断开或连接被重置，属于预期情况
		log.Printf("[rtmp.server] disconnect %s", addr)
	default:
		log.Printf("[rtmp.server] RTMP session error %s: %v", addr, err)
	}
}

// isDisconnect 判断错误是否属于「对端断开」这类正常结束。
func isDisconnect(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscallEPIPE) ||
		errors.Is(err, syscallECONNRESET)
}

// Start 开始监听并在后台接受连接（非阻塞），对应 Python 的 serve_forever(False)。
func (s *RTMPServer) Start() error {
	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rtmp server: 监听 %s 失败: %w", addr, err)
	}
	s.listener = listener

	log.Printf("[rtmp.server] RTMP listening on %s:%d", s.Host, s.Port)
	go s.acceptLoop()
	return nil
}

// acceptLoop 不断接受新连接，并发数由 pool 控制。
func (s *RTMPServer) acceptLoop() {
	for {
		// 先占用一个并发名额，池满时在此阻塞（等价于 gevent Pool 的行为）
		s.pool <- struct{}{}

		conn, err := s.listener.Accept()
		if err != nil {
			<-s.pool
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[rtmp.server] accept 失败: %v", err)
			continue
		}

		go func() {
			defer func() { <-s.pool }()
			s.handleConn(conn)
		}()
	}
}

// Close 关闭监听（正在处理的连接不受影响）。
func (s *RTMPServer) Close() error {
	if s.listener == nil {
		return nil
	}
	return s.listener.Close()
}
