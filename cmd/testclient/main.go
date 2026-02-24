// Package main 提供 Gateway 测试客户端
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
)

// 消息类型常量（与 protocol 包一致）
const (
	// 客户端 → Gateway
	MsgTypeAuth       uint32 = 0x0001
	MsgTypeJoinRoom   uint32 = 0x0002
	MsgTypeLeaveRoom  uint32 = 0x0003
	MsgTypeResume     uint32 = 0x0004
	MsgTypeBizRequest uint32 = 0x0010
	MsgTypeHeartbeat  uint32 = 0x00FF

	// Gateway → 客户端
	MsgTypeAuthResp      uint32 = 0x1001
	MsgTypeJoinRoomResp  uint32 = 0x1002
	MsgTypeLeaveRoomResp uint32 = 0x1003
	MsgTypeResumeResp    uint32 = 0x1004
	MsgTypePushMessage   uint32 = 0x1010
	MsgTypeBizResponse   uint32 = 0x1011
	MsgTypeHeartbeatResp uint32 = 0x10FF
)

const (
	HeaderSize = 16 // 4 + 8 + 4
)

// Frame 消息帧
type Frame struct {
	MsgType uint32
	Seq     uint64
	Payload []byte
}

// 配置
var (
	serverAddr = flag.String("addr", "ws://localhost:8080/ws", "Gateway WebSocket address")
	userID     = flag.String("user", "test_user_001", "User ID")
	roomID     = flag.String("room", "test_room_001", "Room ID to join")
	token      = flag.String("token", "dev_token", "Auth token (dev_xxx for dev mode)")
	heartbeat  = flag.Duration("heartbeat", 30*time.Second, "Heartbeat interval")
	verbose    = flag.Bool("v", false, "Verbose output")
)

var seqCounter uint64

func main() {
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("Starting test client...")
	log.Printf("  Server: %s", *serverAddr)
	log.Printf("  UserID: %s", *userID)
	log.Printf("  RoomID: %s", *roomID)

	// 连接 WebSocket
	conn, _, err := websocket.DefaultDialer.Dial(*serverAddr, nil)
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer conn.Close()

	log.Printf("Connected to server")

	// 认证
	if err := authenticate(conn); err != nil {
		log.Fatalf("Authentication failed: %v", err)
	}

	// 加入房间
	if err := joinRoom(conn); err != nil {
		log.Fatalf("Join room failed: %v", err)
	}

	// 启动心跳
	stopHeartbeat := make(chan struct{})
	go heartbeatLoop(conn, stopHeartbeat)

	// 启动消息接收
	msgCh := make(chan *Frame, 100)
	go receiveLoop(conn, msgCh)

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Client ready. Press Ctrl+C to exit.")
	log.Printf("Listening for push messages...")

	// 主循环
	for {
		select {
		case sig := <-sigCh:
			log.Printf("Received signal: %v, shutting down...", sig)
			close(stopHeartbeat)
			leaveRoom(conn)
			return

		case frame, ok := <-msgCh:
			if !ok {
				log.Printf("Connection closed by server")
				return
			}
			handleMessage(frame)
		}
	}
}

// authenticate 发送认证请求
func authenticate(conn *websocket.Conn) error {
	log.Printf("Authenticating...")

	// 构造认证 payload
	authReq := map[string]interface{}{
		"token":   *token,
		"user_id": *userID,
	}

	payload, err := msgpack.Marshal(authReq)
	if err != nil {
		return fmt.Errorf("marshal auth request: %w", err)
	}

	// 发送认证帧
	frame := &Frame{
		MsgType: MsgTypeAuth,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		return fmt.Errorf("send auth frame: %w", err)
	}

	// 等待响应
	respFrame, err := recvFrame(conn)
	if err != nil {
		return fmt.Errorf("recv auth response: %w", err)
	}

	if respFrame.MsgType != MsgTypeAuthResp {
		return fmt.Errorf("unexpected response type: 0x%04X", respFrame.MsgType)
	}

	// 解析响应
	var resp struct {
		Success   bool   `msgpack:"success"`
		UserID    string `msgpack:"user_id"`
		GatewayID string `msgpack:"gateway_id"`
		Code      int    `msgpack:"code"`
		Message   string `msgpack:"message"`
	}

	if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
		return fmt.Errorf("unmarshal auth response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("auth failed: code=%d, message=%s", resp.Code, resp.Message)
	}

	log.Printf("Authenticated successfully")
	log.Printf("  UserID: %s", resp.UserID)
	log.Printf("  GatewayID: %s", resp.GatewayID)

	return nil
}

// joinRoom 加入房间
func joinRoom(conn *websocket.Conn) error {
	log.Printf("Joining room: %s", *roomID)

	// 构造加入房间 payload
	joinReq := map[string]interface{}{
		"room_id": *roomID,
	}

	payload, err := msgpack.Marshal(joinReq)
	if err != nil {
		return fmt.Errorf("marshal join request: %w", err)
	}

	// 发送加入房间帧
	frame := &Frame{
		MsgType: MsgTypeJoinRoom,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		return fmt.Errorf("send join frame: %w", err)
	}

	// 等待响应
	respFrame, err := recvFrame(conn)
	if err != nil {
		return fmt.Errorf("recv join response: %w", err)
	}

	if respFrame.MsgType != MsgTypeJoinRoomResp {
		return fmt.Errorf("unexpected response type: 0x%04X", respFrame.MsgType)
	}

	// 解析响应
	var resp struct {
		Success bool   `msgpack:"success"`
		RoomID  string `msgpack:"room_id"`
		Code    int    `msgpack:"code"`
		Message string `msgpack:"message"`
	}

	if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
		return fmt.Errorf("unmarshal join response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("join failed: code=%d, message=%s", resp.Code, resp.Message)
	}

	log.Printf("Joined room successfully: %s", resp.RoomID)

	return nil
}

// leaveRoom 离开房间
func leaveRoom(conn *websocket.Conn) {
	log.Printf("Leaving room: %s", *roomID)

	// 构造离开房间 payload
	leaveReq := map[string]interface{}{
		"room_id": *roomID,
	}

	payload, _ := msgpack.Marshal(leaveReq)

	frame := &Frame{
		MsgType: MsgTypeLeaveRoom,
		Seq:     nextSeq(),
		Payload: payload,
	}

	sendFrame(conn, frame)

	// 等待响应（带超时）
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	respFrame, err := recvFrame(conn)
	if err == nil && respFrame.MsgType == MsgTypeLeaveRoomResp {
		log.Printf("Left room successfully")
	}
}

// heartbeatLoop 心跳循环
func heartbeatLoop(conn *websocket.Conn, stop chan struct{}) {
	ticker := time.NewTicker(*heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sendHeartbeat(conn)
		}
	}
}

// sendHeartbeat 发送心跳
func sendHeartbeat(conn *websocket.Conn) {
	hbReq := map[string]interface{}{
		"room_id":   *roomID,
		"timestamp": time.Now().UnixMilli(),
	}

	payload, _ := msgpack.Marshal(hbReq)

	frame := &Frame{
		MsgType: MsgTypeHeartbeat,
		Seq:     nextSeq(),
		Payload: payload,
	}

	if err := sendFrame(conn, frame); err != nil {
		log.Printf("Failed to send heartbeat: %v", err)
		return
	}

	if *verbose {
		log.Printf("Heartbeat sent")
	}
}

// receiveLoop 接收消息循环
func receiveLoop(conn *websocket.Conn, msgCh chan<- *Frame) {
	defer close(msgCh)

	for {
		frame, err := recvFrame(conn)
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("Connection closed normally")
			} else {
				log.Printf("Receive error: %v", err)
			}
			return
		}

		// 心跳响应单独处理
		if frame.MsgType == MsgTypeHeartbeatResp {
			if *verbose {
				log.Printf("Heartbeat response received")
			}
			continue
		}

		msgCh <- frame
	}
}

// handleMessage 处理收到的消息
func handleMessage(frame *Frame) {
	switch frame.MsgType {
	case MsgTypePushMessage:
		handlePushMessage(frame)
	case MsgTypeBizResponse:
		handleBizResponse(frame)
	default:
		log.Printf("Received unknown message type: 0x%04X", frame.MsgType)
	}
}

// handlePushMessage 处理推送消息
func handlePushMessage(frame *Frame) {
	var msg map[string]interface{}
	if err := msgpack.Unmarshal(frame.Payload, &msg); err != nil {
		log.Printf("Failed to unmarshal push message: %v", err)
		return
	}

	log.Printf("Push message received (seq=%d):", frame.Seq)
	for k, v := range msg {
		log.Printf("  %s: %v", k, v)
	}
}

// handleBizResponse 处理业务响应
func handleBizResponse(frame *Frame) {
	var resp map[string]interface{}
	if err := msgpack.Unmarshal(frame.Payload, &resp); err != nil {
		log.Printf("Failed to unmarshal biz response: %v", err)
		return
	}

	log.Printf("Biz response received (seq=%d):", frame.Seq)
	for k, v := range resp {
		log.Printf("  %s: %v", k, v)
	}
}

// sendFrame 发送消息帧
func sendFrame(conn *websocket.Conn, f *Frame) error {
	data := encodeFrame(f)
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

// recvFrame 接收消息帧
func recvFrame(conn *websocket.Conn) (*Frame, error) {
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return decodeFrame(data)
}

// encodeFrame 编码消息帧
func encodeFrame(f *Frame) []byte {
	payloadLen := len(f.Payload)
	buf := make([]byte, HeaderSize+payloadLen)

	binary.BigEndian.PutUint32(buf[0:4], f.MsgType)
	binary.BigEndian.PutUint64(buf[4:12], f.Seq)
	binary.BigEndian.PutUint32(buf[12:16], uint32(payloadLen))

	if payloadLen > 0 {
		copy(buf[HeaderSize:], f.Payload)
	}

	return buf
}

// decodeFrame 解码消息帧
func decodeFrame(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, fmt.Errorf("frame too short: %d bytes", len(data))
	}

	msgType := binary.BigEndian.Uint32(data[0:4])
	seq := binary.BigEndian.Uint64(data[4:12])
	payloadLen := binary.BigEndian.Uint32(data[12:16])

	expectedLen := HeaderSize + int(payloadLen)
	if len(data) < expectedLen {
		return nil, fmt.Errorf("incomplete frame: got %d, expected %d", len(data), expectedLen)
	}

	var payload []byte
	if payloadLen > 0 {
		payload = make([]byte, payloadLen)
		copy(payload, data[HeaderSize:expectedLen])
	}

	return &Frame{
		MsgType: msgType,
		Seq:     seq,
		Payload: payload,
	}, nil
}

// nextSeq 获取下一个序列号
func nextSeq() uint64 {
	return atomic.AddUint64(&seqCounter, 1)
}
